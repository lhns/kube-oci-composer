package buildcontroller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// Tests for spec.push.onConflict on ImageBuild. The registry is stubbed: what is under test is the
// decision the controller reaches from a given registry answer.

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

// TestFailRefusesAConflictingTagAndStartsNoJob: the pre-flight refuses before any Job exists.
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

// TestTheDeprecatedImmutableFieldStillRefuses, or upgrading silently unprotects old objects.
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

// TestOverwriteBuildsOverAnExistingTag: Overwrite (the old immutable: false) still moves the tag.
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

// TestKeepLeavesTheTagAloneAndSaysSo: no build, Ready, and status.conflict records the divergence
// (ADR 0026).
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

// TestAnAbsentTagIsNotAConflict: the ordinary first build must not wedge under the default policy.
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

// TestATagHoldingOurOwnDigestIsNotAConflict for the pre-flight, or every rebuild after a spec
// change would be terminal. applyTags decides exactly afterwards.
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

// TestOnConflictWinsOverTheDeprecatedField pins the precedence rule that keeps upgrades
// non-breaking.
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
		})
	}

	// A nil block must not end up permissive by omission.
	var nilPush *ociv1alpha1.Push
	if got := nilPush.ResolveConflictPolicy(); got != ociv1alpha1.ConflictFail {
		t.Errorf("nil Push = %q, want Fail", got)
	}
}

// TestConflictReportingIsDeterministic: map order must not change which tag is reported.
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

// TestAMissingCacheIsNotImported: BuildKit fails the build on an unresolvable cache import, which
// broke every first build against zot. Asserted on the argv.
func TestAMissingCacheIsNotImported(t *testing.T) {
	obj := buildOf(t, nil)

	absent := strings.Join(buildctlArgs(obj, sampleConfig(), sampleRepo, false), " ")
	if strings.Contains(absent, "--import-cache") {
		t.Error("a cache that does not exist yet is still imported; BuildKit fails the build " +
			"rather than warning, so this is every first build broken")
	}
	if !strings.Contains(absent, "--export-cache") {
		t.Error("export was dropped along with import; nothing would ever create the cache and " +
			"no build would be cached again")
	}

	present := strings.Join(buildctlArgs(obj, sampleConfig(), sampleRepo, true), " ")
	if !strings.Contains(present, "--import-cache") {
		t.Error("a cache that does exist is not imported, so caching never takes effect")
	}
}

// TestBuildsPushOCIMediaTypes: BuildKit's default Docker media types are rejected by OCI-native
// registries such as zot (415).
func TestBuildsPushOCIMediaTypes(t *testing.T) {
	argv := strings.Join(buildctlArgs(buildOf(t, nil), sampleConfig(), sampleRepo, true), " ")

	if !strings.Contains(argv, "oci-mediatypes=true") {
		t.Error("the image exporter does not request OCI media types; an OCI-native registry " +
			"answers the manifest PUT with 415 and the build fails at the very last step")
	}
	if !strings.Contains(argv, "image-manifest=true") {
		t.Error("the cache exporter does not render the cache as an ordinary image manifest; " +
			"BuildKit's default cache format carries a config a conformant registry need not accept")
	}
}

// TestTheOperatorCredentialIsCopiedForTheBuildAndOwnedByIt: a pod mounts Secrets only from its own
// namespace, so the credential is copied there, and its owner reference bounds its lifetime.
func TestTheOperatorCredentialIsCopiedForTheBuildAndOwnedByIt(t *testing.T) {
	reg := tagRegistry(t, nil)
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) { b.Spec.Push.Tags = []string{"v1"} })

	r := harness(t, pinnedFrom, obj)
	obj.Spec.Push.Repository = pushingTo(t, r, reg)
	mustUpdate(t, r, obj)

	host := strings.TrimPrefix(reg.URL, "http://")
	r.Default = recon.DefaultRegistry{Host: host, SecretName: "operator-push", Namespace: "oci-composer"}
	mustCreate(t, r, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "operator-push", Namespace: "oci-composer"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	})

	name, err := r.pushSecretFor(context.Background(), obj, "build-abc")
	if err != nil {
		t.Fatalf("resolving the push secret: %v", err)
	}
	if name != "build-abc-push" {
		t.Fatalf("secret name = %q, want one derived from the Job's", name)
	}

	var copied corev1.Secret
	key := types.NamespacedName{Namespace: obj.Namespace, Name: name}
	if err := r.Get(context.Background(), key, &copied); err != nil {
		t.Fatalf("the copy was not created: %v", err)
	}
	if copied.Namespace != obj.Namespace {
		t.Errorf("the copy is in %q, but a pod can only mount Secrets from its own namespace",
			copied.Namespace)
	}

	// Ownership is what bounds the exposure.
	owners := copied.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "ImageBuild" || owners[0].Name != obj.Name {
		t.Fatalf("owner references = %+v, want the ImageBuild; without one the credential outlives "+
			"the build that needed it", owners)
	}
	if owners[0].Controller == nil || !*owners[0].Controller {
		t.Error("the owner reference is not a controller reference, so garbage collection will not " +
			"remove the copy")
	}
}

// TestNoCredentialIsCopiedForARegistryTheObjectChose: no copy of the operator's credential for a
// registry it does not own.
func TestNoCredentialIsCopiedForARegistryTheObjectChose(t *testing.T) {
	reg := tagRegistry(t, nil)
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.Repository = "attacker.example/x"
	})
	r := harness(t, pinnedFrom, obj)
	r.Default = recon.DefaultRegistry{
		Host:       strings.TrimPrefix(reg.URL, "http://"),
		SecretName: "operator-push",
		Namespace:  "oci-composer",
	}

	name, err := r.pushSecretFor(context.Background(), obj, "build-abc")
	if err != nil {
		t.Fatalf("resolving the push secret: %v", err)
	}
	if name != "" {
		t.Fatalf("secret %q offered for a registry the object chose", name)
	}

	var copied corev1.Secret
	key := types.NamespacedName{Namespace: obj.Namespace, Name: "build-abc-push"}
	if err := r.Get(context.Background(), key, &copied); err == nil {
		t.Error("the operator's credential was copied into the namespace anyway; a build pod there " +
			"could read it and push to the operator's registry")
	}
}

func mustCreate(t *testing.T, r *ImageBuildReconciler, obj client.Object) {
	t.Helper()
	if err := r.Create(context.Background(), obj); err != nil {
		t.Fatalf("creating %T: %v", obj, err)
	}
}
