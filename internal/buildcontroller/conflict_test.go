package buildcontroller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// push.immutable was in this kind's CRD from the day it shipped and NOTHING read it. BuildKit
// pushed type=image,push=true over whatever the tag held, so an operator who set it believed a tag
// could not be remeaned while it silently could. These tests are what make the field real; each
// asserts the behaviour rather than the presence of the code.
//
// The registry is stubbed rather than run, because what is under test is which decision the
// controller reaches from a given registry answer.

// tagRegistry answers HEAD for the tags it is given and 404s for everything else.
func tagRegistry(t *testing.T, tags map[string]string) *httptest.Server {
	t.Helper()
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
		digest, ok := tags[r.URL.Path[i+len("/manifests/"):]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Content-Length", "2")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pushingTo points a build at a stub registry, with that host allowed to use plain HTTP.
func pushingTo(t *testing.T, r *ImageBuildReconciler, srv *httptest.Server) string {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	r.JobConfig.InsecureRegistries = []string{host}
	return host + "/team/app"
}

const otherDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// Fail must refuse BEFORE the Job exists. Checking afterwards would be no check at all: BuildKit
// pushes from inside the Job, so by the time a result comes back the tag has already moved and
// there is no undo.
func TestFailRefusesAConflictingTagAndStartsNoJob(t *testing.T) {
	reg := tagRegistry(t, map[string]string{"v1": otherDigest})
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.OnConflict = ociv1alpha1.ConflictFail
		b.Spec.Push.Tags = []string{"v1"}
	})
	r := harness(t, pinnedFrom, obj)
	obj.Spec.Push.Repository = pushingTo(t, r, reg)
	mustUpdate(t, r, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile returned an error to the caller: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 0 {
		t.Errorf("a Job was created for a build that must be refused; by the time it finishes "+
			"the tag has already moved (%d jobs)", len(jobs))
	}

	got := reload(t, r, obj)
	stalled := conditionOf(got, ociv1alpha1.StalledCondition)
	if stalled == nil || stalled.Status != "True" {
		t.Fatalf("Stalled = %+v, want True: a refused tag is not fixed by retrying", stalled)
	}
	if !strings.Contains(stalled.Message, "already resolves to") {
		t.Errorf("message does not say what the tag holds: %q", stalled.Message)
	}
}

// The deprecated field must keep working, or upgrading silently unprotects every object that set it
// before onConflict existed.
func TestTheDeprecatedImmutableFieldStillRefuses(t *testing.T) {
	reg := tagRegistry(t, map[string]string{"v1": otherDigest})
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.Immutable = ptr.To(true)
		b.Spec.Push.Tags = []string{"v1"}
	})
	r := harness(t, pinnedFrom, obj)
	obj.Spec.Push.Repository = pushingTo(t, r, reg)
	mustUpdate(t, r, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 0 {
		t.Error("immutable: true did not refuse the build; it was inert on this kind before " +
			"onConflict existed and must not stay inert")
	}
}

// Overwrite is the old immutable:false and must still move the tag. It also must not ask the
// registry anything: a permissive policy that broke when reads failed would be a regression for
// every object already using it.
func TestOverwriteBuildsOverAnExistingTag(t *testing.T) {
	reg := tagRegistry(t, map[string]string{"v1": otherDigest})
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.OnConflict = ociv1alpha1.ConflictOverwrite
		b.Spec.Push.Tags = []string{"v1"}
	})
	r := harness(t, pinnedFrom, obj)
	obj.Spec.Push.Repository = pushingTo(t, r, reg)
	mustUpdate(t, r, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1: Overwrite is the old immutable:false and must still build", len(jobs))
	}
}

// Keep leaves the tag alone, runs no build at all, and reports Ready -- and records the divergence,
// because an object that is Ready while not doing what its spec says is the ADR 0026 failure shape
// unless something in status says so.
func TestKeepLeavesTheTagAloneAndSaysSo(t *testing.T) {
	reg := tagRegistry(t, map[string]string{"v1": otherDigest})
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.OnConflict = ociv1alpha1.ConflictKeep
		b.Spec.Push.Tags = []string{"v1"}
	})
	r := harness(t, pinnedFrom, obj)
	obj.Spec.Push.Repository = pushingTo(t, r, reg)
	mustUpdate(t, r, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 0 {
		t.Errorf("Keep started a build; the point is that the content is already published (%d jobs)",
			len(jobs))
	}

	got := reload(t, r, obj)
	ready := conditionOf(got, ociv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != "True" {
		t.Fatalf("Ready = %+v, want True: an existing spec-hash tag is not a failure", ready)
	}
	c := got.Status.Conflict
	if c == nil {
		t.Fatal("status.conflict is empty: the object reads healthy while not doing what its " +
			"spec asks, and nothing anywhere says the two disagree")
	}
	if c.Tag != "v1" || c.Existing != otherDigest {
		t.Errorf("conflict = %+v, want tag v1 at %s", c, otherDigest)
	}
	if !strings.Contains(ready.Message, "v1") {
		t.Errorf("Ready message does not mention the kept tag: %q", ready.Message)
	}
}

// A tag that does not exist yet is the ordinary first build, and must not be mistaken for a
// conflict. Getting this wrong would wedge every new object under the default policy.
func TestAnAbsentTagIsNotAConflict(t *testing.T) {
	reg := tagRegistry(t, nil)
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.OnConflict = ociv1alpha1.ConflictFail
		b.Spec.Push.Tags = []string{"v1"}
	})
	r := harness(t, pinnedFrom, obj)
	obj.Spec.Push.Repository = pushingTo(t, r, reg)
	mustUpdate(t, r, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1: a tag that does not exist is not a conflict", len(jobs))
	}
}

// A tag pointing at THIS object's own recorded digest is its own last build, not somebody else's
// content. Treating it as a conflict would make every rebuild after a spec change terminal.
func TestATagHoldingOurOwnDigestIsNotAConflict(t *testing.T) {
	const ours = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	reg := tagRegistry(t, map[string]string{"v1": ours})
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.OnConflict = ociv1alpha1.ConflictFail
		b.Spec.Push.Tags = []string{"v1"}
		b.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: ours}
	})
	r := harness(t, pinnedFrom, obj)
	obj.Spec.Push.Repository = pushingTo(t, r, reg)
	mustUpdate(t, r, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1: a tag holding this object's own digest is not a conflict", len(jobs))
	}
}

// The precedence rule, which is what makes the upgrade non-breaking in both directions.
func TestOnConflictWinsOverTheDeprecatedField(t *testing.T) {
	for _, tc := range []struct {
		name       string
		explicit   ociv1alpha1.TagConflictPolicy
		deprecated *bool
		want       ociv1alpha1.TagConflictPolicy
	}{
		{"neither set defaults to the safe answer", "", nil, ociv1alpha1.ConflictFail},
		{"immutable true means Fail", "", ptr.To(true), ociv1alpha1.ConflictFail},
		{"immutable false means Overwrite", "", ptr.To(false), ociv1alpha1.ConflictOverwrite},
		{"onConflict wins", ociv1alpha1.ConflictKeep, ptr.To(true), ociv1alpha1.ConflictKeep},
		{"onConflict wins even against a false", ociv1alpha1.ConflictFail, ptr.To(false), ociv1alpha1.ConflictFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			push := &ociv1alpha1.Push{OnConflict: tc.explicit, Immutable: tc.deprecated}
			if got := push.ResolveConflictPolicy(); got != tc.want {
				t.Errorf("Push = %q, want %q", got, tc.want)
			}
			pub := &ociv1alpha1.Publish{OnConflict: tc.explicit, Immutable: tc.deprecated}
			if got := pub.ResolveConflictPolicy(); got != tc.want {
				t.Errorf("Publish = %q, want %q; the two kinds must not disagree", got, tc.want)
			}
		})
	}

	// A nil block must not end up permissive by omission.
	var nilPush *ociv1alpha1.Push
	if got := nilPush.ResolveConflictPolicy(); got != ociv1alpha1.ConflictFail {
		t.Errorf("nil Push = %q, want Fail", got)
	}
}

// The shared helper the policy is applied through. Map iteration order must not leak into which tag
// gets reported, or an unchanged object would name a different tag on each reconcile.
func TestConflictReportingIsDeterministic(t *testing.T) {
	p := recon.Published{
		Tags:   map[string]string{"a": otherDigest, "b": otherDigest, "c": otherDigest},
		Wanted: 3,
	}
	first, _ := p.Conflicts([]string{"a", "b", "c"}, "sha256:zzzz")
	for i := 0; i < 50; i++ {
		if tag, _ := p.Conflicts([]string{"a", "b", "c"}, "sha256:zzzz"); tag != first {
			t.Fatalf("reported tag varies between calls: %q then %q", first, tag)
		}
	}
	if first != "a" {
		t.Errorf("reported %q, want the first tag in the spec's order", first)
	}
}

func mustUpdate(t *testing.T, r *ImageBuildReconciler, obj *ociv1alpha1.ImageBuild) {
	t.Helper()
	if err := r.Update(context.Background(), obj); err != nil {
		t.Fatalf("updating the object: %v", err)
	}
}

// A missing build cache must never fail a build, and for as long as the e2e ran against registry:2
// this was true only by accident.
//
// BuildKit configures the registry cache importer eagerly and treats a reference it cannot resolve
// as a fatal error rather than a warning. registry:2's answer for a missing manifest happened to be
// one BuildKit tolerated; zot's is not, and every FIRST build failed the moment the e2e registry
// changed -- with an error about a cache, on a build that had no cache because it had never run.
//
// Asserted on the rendered argv rather than through a registry, because what went wrong was which
// flags were passed, not what any registry replied.
func TestAMissingCacheIsNotImported(t *testing.T) {
	obj := buildOf(t, nil)

	absent := strings.Join(buildctlArgs(obj, sampleConfig(), false), " ")
	if strings.Contains(absent, "--import-cache") {
		t.Error("a cache that does not exist yet is still imported; BuildKit fails the build " +
			"rather than warning, so this is every first build broken")
	}
	if !strings.Contains(absent, "--export-cache") {
		t.Error("export was dropped along with import; nothing would ever create the cache and " +
			"no build would be cached again")
	}

	present := strings.Join(buildctlArgs(obj, sampleConfig(), true), " ")
	if !strings.Contains(present, "--import-cache") {
		t.Error("a cache that does exist is not imported, so caching never takes effect")
	}
}

// BuildKit emits DOCKER media types unless told otherwise, and an OCI-native registry answers a
// manifest PUT with 415 Unsupported Media Type. That is what zot did, and it failed every build.
//
// The Docker types were never chosen here -- they were BuildKit's default and nothing had
// contradicted it. The composer already writes OCI manifests, so this also stops the two kinds
// putting different media types into one registry.
func TestBuildsPushOCIMediaTypes(t *testing.T) {
	argv := strings.Join(buildctlArgs(buildOf(t, nil), sampleConfig(), true), " ")

	if !strings.Contains(argv, "oci-mediatypes=true") {
		t.Error("the image exporter does not request OCI media types; an OCI-native registry " +
			"answers the manifest PUT with 415 and the build fails at the very last step")
	}
	if !strings.Contains(argv, "image-manifest=true") {
		t.Error("the cache exporter does not render the cache as an ordinary image manifest; " +
			"BuildKit's default cache format carries a config a conformant registry need not accept")
	}
}
