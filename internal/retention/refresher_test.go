package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// The refresh is the whole retention guarantee (ADR 0031), and it fails UNSAFE: if it silently stops
// doing its job, nothing breaks until the registry's window elapses and live images are deleted.
// There is no reconcile that goes red, no object that stalls, no pull that fails — until one day
// every pull fails at once.
//
// That is why these tests assert on the REQUESTS the refresher makes rather than on its return
// value. A refresher that returns a healthy-looking Result while touching nothing is precisely the
// failure this has to catch, and it would satisfy any assertion made against its own report.

// recordingRegistry answers manifest requests and remembers every path asked for.
type recordingRegistry struct {
	*httptest.Server
	mu      sync.Mutex
	got     []string
	missing map[string]bool
}

func newRegistry(t *testing.T, missing ...string) *recordingRegistry {
	t.Helper()
	reg := &recordingRegistry{missing: map[string]bool{}}
	for _, m := range missing {
		reg.missing[m] = true
	}

	reg.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		i := strings.Index(r.URL.Path, "/manifests/")
		if i < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		ref := r.URL.Path[i+len("/manifests/"):]

		reg.mu.Lock()
		reg.got = append(reg.got, r.Method+" "+ref)
		missing := reg.missing[ref]
		reg.mu.Unlock()

		if missing {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// The body a digest reference resolves to must actually hash to that digest, because
		// go-containerregistry verifies it -- as any correct client does. A stub returning a fixed
		// body for every digest is not a registry, it is a way to make the code under test look
		// broken.
		body := manifestFor(ref)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digestOf(body))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(reg.Close)
	return reg
}

// manifests maps a digest to the body that hashes to it, plus a default for tag lookups.
var manifests = map[string]string{}

// manifestFor returns the body a reference resolves to.
func manifestFor(ref string) string {
	if body, ok := manifests[ref]; ok {
		return body
	}
	return taggedManifest
}

// digestOf is the manifest digest of a body, which is what a digest reference names.
func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func manifestBody(marker string) string {
	return `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":2,` +
		`"digest":"` + emptyDigest + `"},"layers":[],"annotations":{"m":"` + marker + `"}}`
}

func (reg *recordingRegistry) requests() []string {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return append([]string(nil), reg.got...)
}

func (reg *recordingRegistry) host() string { return strings.TrimPrefix(reg.URL, "http://") }

const emptyDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// Two real manifests, and the digests that actually name them.
var (
	bodyA          = manifestBody("a")
	bodyB          = manifestBody("b")
	taggedManifest = bodyA
	digestA        = digestOf(bodyA)
	digestB        = digestOf(bodyB)
)

func init() {
	manifests[digestA] = bodyA
	manifests[digestB] = bodyB
}

type allReconciled struct{ pending []string }

func (a allReconciled) Pending(context.Context) ([]string, error) { return a.pending, nil }

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("client-go scheme: %v", err)
	}
	if err := ociv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("api scheme: %v", err)
	}
	return s
}

// publicHost is what a workload is told to pull from, and it resolves NOWHERE -- no test server
// listens on it and it is not in InsecureRegistries.
//
// Every fixture below stores its tags through it, because that is what both controllers write into
// status.artifact.tags and status.history[].tags. Using the reachable host in both places is what
// let the refresher trust a stored tag for a whole release: the two could not disagree, so the bug
// had nowhere to show. ADR 0048.
const publicHost = "oci-composer.internal:30500"

// publicTag writes a tag the way status carries it: qualified with the public host.
func publicTag(tag string) string { return publicHost + "/team/app:" + tag }

// buildWith returns an ImageBuild publishing the given history to the registry.
func buildWith(reg *recordingRegistry, name string, history []ociv1alpha1.BuildRecord,
	artifact *ociv1alpha1.ArtifactStatus) *ociv1alpha1.ImageBuild {

	obj := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
	}
	obj.Spec.Push = &ociv1alpha1.Push{Repository: reg.host() + "/team/app"}
	obj.Status.History = history
	obj.Status.Artifact = artifact
	return obj
}

// Every retained record must be refreshed under BOTH its digest and its tags.
//
// Measured against a real registry (test/e2e/retention_test.go): pulling only the digest keeps the
// CONTENT alive and lets the TAG be collected, because a registry can govern tagged and untagged
// manifests by different rules. A refresh keeps alive exactly what it asks for.
func TestEveryRetainedReferenceIsRefreshed(t *testing.T) {
	reg := newRegistry(t)

	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestA, Tags: []string{publicTag("v1"), publicTag("latest")}},
		{Digest: digestB, Tags: []string{publicTag("v0")}},
	}, nil)

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{},
		Recorder:           record.NewFakeRecorder(50),
		InsecureRegistries: []string{reg.host()},
	}

	res, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	got := reg.requests()
	for _, want := range []string{digestA, digestB, "v1", "latest", "v0"} {
		if !containsRef(got, want) {
			t.Errorf("%s was never refreshed; whatever is not pulled is what the registry "+
				"collects\nrequests: %v", want, got)
		}
	}
	if res.Refreshed != 5 {
		t.Errorf("refreshed = %d, want 5 (two digests and three tags)", res.Refreshed)
	}
}

// CONDITION 2 of ADR 0031, named there as the most likely implementation mistake.
//
// An object Stalled on a spec error must keep refreshing what it already published. Those images may
// be running right now, and stalling is precisely when nobody is watching the object. A refresh
// gated on a successful reconcile would delete the images of every object with a broken spec, one
// retention window after it broke.
func TestAStalledObjectStillRefreshes(t *testing.T) {
	reg := newRegistry(t)

	obj := buildWith(reg, "stalled", []ociv1alpha1.BuildRecord{
		{Digest: digestA, Tags: []string{publicTag("v1")}},
	}, nil)
	obj.Status.Conditions = []metav1.Condition{
		{Type: ociv1alpha1.StalledCondition, Status: metav1.ConditionTrue,
			Reason: ociv1alpha1.ReasonInvalidSpec, Message: "spec.push.repository is invalid",
			LastTransitionTime: metav1.Now()},
		{Type: ociv1alpha1.ReadyCondition, Status: metav1.ConditionFalse,
			Reason: ociv1alpha1.ReasonInvalidSpec, Message: "spec.push.repository is invalid",
			LastTransitionTime: metav1.Now()},
	}

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{},
		Recorder:           record.NewFakeRecorder(50),
		InsecureRegistries: []string{reg.host()},
	}

	if _, err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	got := reg.requests()
	if !containsRef(got, digestA) || !containsRef(got, "v1") {
		t.Fatalf("a Stalled object stopped being refreshed, so the images it already published "+
			"will be deleted one retention window after its spec broke\nrequests: %v", got)
	}
}

// A partial view must refresh NOTHING rather than most things.
//
// The collector's version of this rail protects against sweeping something live. This one protects
// against the opposite and quieter failure: an object missing from the view is an object whose
// images stop being kept alive, with the symptom arriving a window later and nothing connecting it
// back to the cause.
func TestAPartialViewRefreshesNothing(t *testing.T) {
	reg := newRegistry(t)

	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestA, Tags: []string{publicTag("v1")}},
	}, nil)

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{pending: []string{"team-a/not-seen-yet"}},
		Recorder:           record.NewFakeRecorder(50),
		InsecureRegistries: []string{reg.host()},
	}

	res, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if !res.Skipped {
		t.Error("a cycle ran against an incomplete view; some objects would silently go unrefreshed")
	}
	if got := reg.requests(); len(got) != 0 {
		t.Errorf("requests were made despite skipping: %v", got)
	}
}

// A reference the registry no longer has means the guarantee has ALREADY been broken by something
// else. Counted apart from an unreachable registry, because they are different alarms: one says the
// protection failed, the other says it might.
func TestAMissingReferenceIsReportedSeparately(t *testing.T) {
	reg := newRegistry(t, digestB)

	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestA, Tags: []string{publicTag("v1")}},
		{Digest: digestB},
	}, nil)

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{},
		Recorder:           record.NewFakeRecorder(50),
		InsecureRegistries: []string{reg.host()},
	}

	res, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if res.NotFound != 1 {
		t.Errorf("notFound = %d, want 1: a reference that is gone must be distinguishable from a "+
			"registry that did not answer", res.NotFound)
	}
}

// CONDITION 4 of ADR 0031. A design that fails unsafe needs monitoring in a way that a fail-safe one
// does not: sustained failure has to be loud well before the window elapses, because the alternative
// to noticing is deletion.
func TestSustainedFailureRaisesAnEvent(t *testing.T) {
	reg := newRegistry(t, digestA)
	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{{Digest: digestA}}, nil)

	events := record.NewFakeRecorder(50)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{},
		Recorder:           events,
		InsecureRegistries: []string{reg.host()},
	}

	// One failure is a rolling restart, not a problem worth waking anyone for.
	if _, err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if len(events.Events) != 0 {
		t.Error("a single failed cycle raised an event; that trains operators to ignore the signal")
	}

	for i := 1; i < DegradedAfter; i++ {
		if _, err := r.RefreshOnce(context.Background()); err != nil {
			t.Fatalf("refreshing: %v", err)
		}
	}

	select {
	case ev := <-events.Events:
		if !strings.Contains(ev, ociv1alpha1.ReasonRetentionDegraded) {
			t.Errorf("event = %q, want one naming %s", ev, ociv1alpha1.ReasonRetentionDegraded)
		}
	default:
		t.Fatalf("no event after %d consecutive failures; the failure mode of this component is "+
			"silence followed by deletion", DegradedAfter)
	}
}

// A recovered object must stop being reported, or the warning becomes permanent and worthless.
func TestRecoveryClearsTheFailureCount(t *testing.T) {
	reg := newRegistry(t, digestA)
	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{{Digest: digestA}}, nil)

	events := record.NewFakeRecorder(50)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{},
		Recorder:           events,
		InsecureRegistries: []string{reg.host()},
	}

	for i := 0; i < DegradedAfter-1; i++ {
		if _, err := r.RefreshOnce(context.Background()); err != nil {
			t.Fatalf("refreshing: %v", err)
		}
	}

	// The registry comes back.
	reg.mu.Lock()
	reg.missing = map[string]bool{}
	reg.mu.Unlock()

	if _, err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	// ...and then fails once more. That must not immediately re-trip the threshold.
	reg.mu.Lock()
	reg.missing = map[string]bool{digestA: true}
	reg.mu.Unlock()
	if _, err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	if len(events.Events) != 0 {
		t.Error("a single failure after a recovery raised the degraded event; the count did not " +
			"reset, so the warning would fire on any object that has ever failed")
	}
}

// An object with no repository AND no default registry configured has nothing to refresh, and must
// not be counted as a failure.
//
// This used to be the serving case -- an object served from the embedded endpoint had no external
// registry to convince of anything. That endpoint is gone (ADR 0035), so the only way to reach this
// state now is an operator who has configured no default registry, where the object is Pending and
// has never published anything.
func TestAnObjectWithNowhereToPublishIsNotAFailure(t *testing.T) {
	reg := newRegistry(t)

	obj := &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{Name: "nowhere", Namespace: "team-a"},
	}
	obj.Status.History = []ociv1alpha1.BuildRecord{{Digest: digestA}}

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	r := &Refresher{
		Client:   c,
		Source:   sourceFor(obj, c),
		Pending:  allReconciled{},
		Recorder: record.NewFakeRecorder(50),
		// No Default: nothing is configured, so there is nowhere for this object to publish.
	}

	res, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if res.Failed != 0 {
		t.Errorf("failed = %d, want 0: an object with nowhere to publish is not a refresh failure",
			res.Failed)
	}
	if len(reg.requests()) != 0 {
		t.Error("an object with no registry produced registry traffic")
	}
}

// Credentials come from the object's own push Secret and are never taken from the spec.
func TestCredentialsComeFromTheObjectsSecret(t *testing.T) {
	reg := newRegistry(t)
	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{{Digest: digestA}}, nil)
	obj.Spec.Push.SecretRef = &ociv1alpha1.LocalObjectReference{Name: "push-creds"}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "push-creds", Namespace: "team-a"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"` + reg.host() +
				`":{"username":"u","password":"p"}}}`),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj, secret).Build()
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{},
		Recorder:           record.NewFakeRecorder(50),
		InsecureRegistries: []string{reg.host()},
	}

	res, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if res.Refreshed != 1 {
		t.Errorf("refreshed = %d, want 1; the credential was not usable", res.Refreshed)
	}
}

// A missing Secret is a failure to refresh, not a silent skip. Skipping would mean the object's
// images quietly stop being protected while nothing anywhere says so.
func TestAnUnusableSecretIsAFailureNotASkip(t *testing.T) {
	reg := newRegistry(t)
	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{{Digest: digestA}}, nil)
	obj.Spec.Push.SecretRef = &ociv1alpha1.LocalObjectReference{Name: "absent"}

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{},
		Recorder:           record.NewFakeRecorder(50),
		InsecureRegistries: []string{reg.host()},
	}

	res, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if res.Failed == 0 {
		t.Error("a missing push Secret was treated as nothing to do; the object's images would " +
			"stop being protected with no signal at all")
	}
}

// Refusing to run without a Pending gate, rather than running unguarded.
func TestNoPendingGateRefusesToRun(t *testing.T) {
	r := &Refresher{Client: fake.NewClientBuilder().WithScheme(scheme(t)).Build()}
	if _, err := r.RefreshOnce(context.Background()); err == nil {
		t.Error("refreshed without a completeness gate; a partial view under-refreshes silently")
	}
}

func containsRef(requests []string, want string) bool {
	for _, r := range requests {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

// sourceFor picks the source for whichever kind the test built, so each test stays about retention
// rather than about which component owns which list.
func sourceFor(obj client.Object, c client.Client) Source {
	if _, ok := obj.(*ociv1alpha1.ImageBuild); ok {
		return BuildSource{Client: c}
	}
	return CompositionSource{Client: c}
}

// TestEveryReferenceIsBuiltFromTheResolvedRepository is the invariant, asserted directly.
//
// Retention runs in-cluster. If a reference it dials came from anywhere but the repository it
// resolved, it is addressing a host it may not be able to reach -- which is the whole of ADR 0048.
// One assertion, and the class of bug cannot come back.
func TestEveryReferenceIsBuiltFromTheResolvedRepository(t *testing.T) {
	const repo = "registry.svc.cluster.local:5000/team-a/app"

	refs := refsOf(repo,
		&ociv1alpha1.ArtifactStatus{
			Digest: digestA,
			// As status carries them: through the public host.
			Tags: []string{publicTag("v1"), publicTag("latest")},
		},
		[]ociv1alpha1.BuildRecord{
			{Digest: digestB, Tags: []string{publicTag("v0")}},
			// A bare tag, as a hand-edited or pre-0.5.0 object may hold.
			{Digest: digestA, Tags: []string{"legacy"}},
		})

	if len(refs) == 0 {
		t.Fatal("no references at all")
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, repo+":") && !strings.HasPrefix(ref, repo+"@") {
			t.Errorf("reference %q was not built from the resolved repository %q; retention would "+
				"dial a host it has no reason to be able to resolve", ref, repo)
		}
	}
	for _, want := range []string{repo + ":v1", repo + ":latest", repo + ":v0", repo + ":legacy"} {
		var found bool
		for _, ref := range refs {
			if ref == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is missing; whatever is not refreshed is what the registry collects\ngot: %v",
				want, refs)
		}
	}
}

// TestABareTagIsRecoveredFromWhateverStatusHolds pins the parse, because the rule is not obvious:
// a host carries a colon of its own and only the LAST one, after the last slash, introduces a tag.
func TestABareTagIsRecoveredFromWhateverStatusHolds(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"already bare", "v1", "v1"},
		{"public host with a port", "oci-composer.internal:30500/team-a/app:v1", "v1"},
		{"in-cluster host with a port", "registry.svc:5000/team-a/app:sb6b025064f7ba9bc", "sb6b025064f7ba9bc"},
		{"no port", "ghcr.io/example/app:v1", "v1"},
		{"empty", "", ""},
		// A repository and no tag names nothing that was ever published, so it contributes
		// nothing rather than being appended to repo as if it were a tag.
		{"repository with no tag", "oci-composer.internal:30500/team-a/app", ""},
		{"host and port only", "registry.svc:5000/app", ""},
		{"a digest is not a tag", "ghcr.io/example/app@" + digestA, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bareTag(tc.in); got != tc.want {
				t.Errorf("bareTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTagsStoredWithAnUnresolvableHostStillRefresh is the reported bug, end to end.
//
// A live 0.5.0 cluster reported "5 of 9 references" failing every hour for three days, with
// "lookup oci-composer.internal: no such host". Digests refreshed and tags did not, because the
// stored tag carried the public host and the refresher used it verbatim. One repository lost every
// tag it had; the manifest survived only as an untagged blob, which is what deleteUntagged reclaims.
//
// The public host here resolves nowhere, exactly as it does not resolve from a pod.
func TestTagsStoredWithAnUnresolvableHostStillRefresh(t *testing.T) {
	reg := newRegistry(t)

	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestB, Tags: []string{publicTag("v0")}},
	}, &ociv1alpha1.ArtifactStatus{
		Digest: digestA,
		Ref:    publicHost + "/team/app@" + digestA,
		Tags:   []string{publicTag("v1")},
	})

	rec := record.NewFakeRecorder(50)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{},
		Recorder:           rec,
		InsecureRegistries: []string{reg.host()},
	}

	res, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	if res.Failed != 0 {
		t.Errorf("failed = %d, want 0: every reference is reachable at the resolved repository",
			res.Failed)
	}
	if res.Refreshed != res.References {
		t.Errorf("refreshed %d of %d references", res.Refreshed, res.References)
	}
	// The tags specifically, since those are the ones that were being lost.
	for _, want := range []string{"v1", "v0"} {
		if !containsRef(reg.requests(), want) {
			t.Errorf("tag %q was never refreshed, so the registry would collect it\nrequests: %v",
				want, reg.requests())
		}
	}
	select {
	case ev := <-rec.Events:
		t.Errorf("a healthy refresh raised an event: %s", ev)
	default:
	}
}
