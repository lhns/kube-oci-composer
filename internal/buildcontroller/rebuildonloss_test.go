package buildcontroller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// registryAnswering serves HEAD for the named references and answers everything else with `code`.
//
// The code matters as much as the set: a 404 is a loss and anything else is a question the registry
// declined to answer, and stillPublished must act on exactly one of those.
func registryAnswering(t *testing.T, code int, present ...string) *httptest.Server {
	t.Helper()
	have := map[string]bool{}
	for _, p := range present {
		have[p] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		i := strings.Index(r.URL.Path, "/manifests/")
		if i < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if have[r.URL.Path[i+len("/manifests/"):]] {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digestOfNothing)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const digestOfNothing = "sha256:" +
	"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// builtAndPublished returns an object that has already published, so the short-circuit applies.
func builtAndPublished(host, digest string) *ociv1alpha1.ImageBuild {
	obj := sampleBuild()
	obj.Spec.Push = &ociv1alpha1.Push{Repository: host + "/team/app", Tags: []string{"v1"}}
	obj.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: digest}
	return obj
}

func refresherFor(t *testing.T, srv *httptest.Server) *ImageBuildReconciler {
	t.Helper()
	cfg := sampleConfig()
	cfg.InsecureRegistries = []string{strings.TrimPrefix(srv.URL, "http://")}
	return &ImageBuildReconciler{JobConfig: cfg, Recorder: record.NewFakeRecorder(20)}
}

// TestAPresentArtifactIsNotRebuilt is the "only if missing" half.
func TestAPresentArtifactIsNotRebuilt(t *testing.T) {
	srv := registryAnswering(t, http.StatusNotFound, digestOfNothing, "v1")
	host := strings.TrimPrefix(srv.URL, "http://")

	r := refresherFor(t, srv)
	if !r.stillPublished(context.Background(), builtAndPublished(host, digestOfNothing)) {
		t.Error("an artifact that is entirely present was reported missing; every reconcile would " +
			"start a build")
	}
}

// TestAMissingDigestIsMissing — the content itself is gone.
func TestAMissingDigestIsMissing(t *testing.T) {
	srv := registryAnswering(t, http.StatusNotFound, "v1")
	host := strings.TrimPrefix(srv.URL, "http://")

	r := refresherFor(t, srv)
	if r.stillPublished(context.Background(), builtAndPublished(host, digestOfNothing)) {
		t.Error("a deleted manifest was reported present")
	}
}

// TestAMissingTagIsMissing is the shape the field report actually took.
//
// github-runner kept its manifest and lost every tag. An untagged manifest is exactly what the
// shipped deleteUntagged policy reclaims next, so a lost tag is a loss in progress rather than a
// cosmetic one -- checking only the digest would have called that healthy.
func TestAMissingTagIsMissing(t *testing.T) {
	srv := registryAnswering(t, http.StatusNotFound, digestOfNothing)
	host := strings.TrimPrefix(srv.URL, "http://")

	r := refresherFor(t, srv)
	if r.stillPublished(context.Background(), builtAndPublished(host, digestOfNothing)) {
		t.Error("the manifest survives by digest but every tag is gone, and that was called " +
			"healthy; deleteUntagged reclaims it next")
	}
}

// TestAnUnreachableRegistryNeverTriggersARebuild is the safety property, and the one worth having.
//
// Only a definite 404 is a loss. If any other error counted, a single registry outage would start
// a build for EVERY ImageBuild in the cluster at once -- the worst available response to a registry
// that is already struggling, and self-sustaining once those builds start pushing.
func TestAnUnreachableRegistryNeverTriggersARebuild(t *testing.T) {
	for _, code := range []int{
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
	} {
		srv := registryAnswering(t, code)
		host := strings.TrimPrefix(srv.URL, "http://")

		r := refresherFor(t, srv)
		if !r.stillPublished(context.Background(), builtAndPublished(host, digestOfNothing)) {
			t.Errorf("HTTP %d was treated as a loss; one registry outage would rebuild the whole "+
				"cluster", code)
		}
	}
}

// TestAnObjectThatNeverPublishedIsNotChecked — there is nothing to have lost, and no round trip
// worth spending to discover that.
func TestAnObjectThatNeverPublishedIsNotChecked(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	obj := builtAndPublished(host, digestOfNothing)
	obj.Status.Artifact = nil

	r := refresherFor(t, srv)
	if !r.stillPublished(context.Background(), obj) {
		t.Error("an object with no artifact was reported as having lost one")
	}
	if reached {
		t.Error("the registry was queried about an object that has never published")
	}
}

// TestTheLossEventSaysTheDigestChanges — the rebuild REPLACES, it does not restore, and anything
// pinned to the old digest is not helped. Silence there would be the dishonest part.
func TestTheLossEventSaysTheDigestChanges(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	obj := builtAndPublished("example:5000", digestOfNothing)
	recon.Event(rec, obj, corev1.EventTypeWarning, ociv1alpha1.ReasonArtifactLost,
		"placeholder")

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, ociv1alpha1.ReasonArtifactLost) {
			t.Errorf("event = %q, want one naming %s", ev, ociv1alpha1.ReasonArtifactLost)
		}
	default:
		t.Fatal("no event was recorded")
	}
}

// TestALostArtifactStartsANewBuild is the wiring, which the tests above deliberately do not cover:
// each of them exercises stillPublished directly and would pass even if nothing ever called it.
//
// The short-circuit returned on unchanged inputs alone, so a build whose image had been reclaimed
// sat reporting Ready forever. Falling through IS the trigger -- the very next lines are
// currentJob, checkTagConflict and startBuild -- so the assertion is that a Job appears.
func TestALostArtifactStartsANewBuild(t *testing.T) {
	srv := registryAnswering(t, http.StatusNotFound)
	host := strings.TrimPrefix(srv.URL, "http://")

	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push = &ociv1alpha1.Push{Repository: host + "/team/app", Tags: []string{"v1"}}
		// Already built, and the inputs have not changed: without the existence check this
		// reconcile does nothing at all.
		b.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: digestOfNothing}
	})

	r := harness(t, pinnedFrom, obj)
	r.JobConfig.InsecureRegistries = []string{host}
	r.Recorder = record.NewFakeRecorder(20)

	// The recorded hash has to be the one this spec produces, or the object looks changed and would
	// rebuild for the ordinary reason instead of the one under test.
	inputs, _, err := r.resolveInputs(context.Background(), obj)
	if err != nil {
		t.Fatalf("resolving inputs: %v", err)
	}
	obj.Status.InputHash = inputs.Hash()
	if err := r.Status().Update(context.Background(), obj); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(obj.Namespace)); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("got %d jobs, want 1: an image that is gone from the registry must be rebuilt, "+
			"not reported Ready forever", len(jobs.Items))
	}

	// And it must say so, because the rebuild replaces rather than restores.
	var said bool
	for {
		select {
		case ev := <-r.Recorder.(*record.FakeRecorder).Events:
			if strings.Contains(ev, ociv1alpha1.ReasonArtifactLost) &&
				strings.Contains(ev, "DIFFERENT digest") {
				said = true
			}
			continue
		default:
		}
		break
	}
	if !said {
		t.Error("the rebuild was silent about producing a different digest, which is the one thing " +
			"an operator pinning that digest needs to know")
	}
}
