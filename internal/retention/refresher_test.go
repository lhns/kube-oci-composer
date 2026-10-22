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

// The refresh fails UNSAFE (ADR 0031), so these tests assert on the REQUESTS the refresher makes,
// not only on its Result: a refresher that reports success while touching nothing must still fail.

// recordingRegistry answers manifest requests and remembers every path asked for.
type recordingRegistry struct {
	*httptest.Server
	mu      sync.Mutex
	got     []string
	missing map[string]bool
	broken  map[string]bool
}

func newRegistry(t *testing.T, missing ...string) *recordingRegistry {
	t.Helper()
	reg := &recordingRegistry{missing: map[string]bool{}, broken: map[string]bool{}}
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
		broken := reg.broken[ref]
		reg.mu.Unlock()

		// A 500 and a 404 are different alarms (ADR 0049); the stub produces both.
		if broken {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if missing {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// A digest reference must resolve to a body that hashes to it; the client verifies it.
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

// publicHost is what a workload is told to pull from, and it resolves NOWHERE. Fixtures store their
// tags through it, as both controllers do in status, so a refresher that trusts a stored tag's host
// fails here (ADR 0048).
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

// Every retained record must be refreshed under BOTH its digest and its tags: pulling only the
// digest lets the tag be collected (measured in test/e2e/retention_test.go).
func TestEveryRetainedReferenceIsRefreshed(t *testing.T) {
	reg := newRegistry(t)

	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestA, Tags: []string{publicTag("v1"), publicTag("latest")}},
		{Digest: digestB, Tags: []string{publicTag("v0")}},
	}, nil)

	r, _ := refresherFor(t, reg.host(), obj)

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

// Condition 2 of ADR 0031: an object Stalled on a spec error must keep refreshing what it already
// published, since those images may be running.
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

	r, _ := refresherFor(t, reg.host(), obj)

	if _, err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	got := reg.requests()
	if !containsRef(got, digestA) || !containsRef(got, "v1") {
		t.Fatalf("a Stalled object stopped being refreshed, so the images it already published "+
			"will be deleted one retention window after its spec broke\nrequests: %v", got)
	}
}

// A partial view must refresh NOTHING rather than most things, since an object missing from the view
// would silently stop being kept alive.
func TestAPartialViewRefreshesNothing(t *testing.T) {
	reg := newRegistry(t)

	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestA, Tags: []string{publicTag("v1")}},
	}, nil)

	r, _ := refresherFor(t, reg.host(), obj)
	r.Pending = allReconciled{pending: []string{"team-a/not-seen-yet"}}

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

// A reference the registry no longer has is counted apart from an unreachable registry: one says the
// protection already failed, the other that it might.
func TestAMissingReferenceIsReportedSeparately(t *testing.T) {
	reg := newRegistry(t, digestB)

	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestA, Tags: []string{publicTag("v1")}},
		{Digest: digestB},
	}, nil)

	r, _ := refresherFor(t, reg.host(), obj)

	res, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if res.NotFound != 1 {
		t.Errorf("notFound = %d, want 1: a reference that is gone must be distinguishable from a "+
			"registry that did not answer", res.NotFound)
	}
}

// Condition 4 of ADR 0031: sustained failure must be loud well before the window elapses.
func TestSustainedFailureRaisesAnEvent(t *testing.T) {
	// A transient 500, not a 404: only a transient error sustains Degraded (ADR 0049).
	reg := newRegistry(t)
	reg.setBroken(digestA)
	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{{Digest: digestA}}, nil)

	r, events := refresherFor(t, reg.host(), obj)

	// One failure is not worth an event.
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
	reg := newRegistry(t)
	reg.setBroken(digestA)
	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{{Digest: digestA}}, nil)

	r, events := refresherFor(t, reg.host(), obj)

	for i := 0; i < DegradedAfter-1; i++ {
		if _, err := r.RefreshOnce(context.Background()); err != nil {
			t.Fatalf("refreshing: %v", err)
		}
	}

	// The registry comes back.
	reg.setBroken()

	if _, err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	// ...and then fails once more. That must not immediately re-trip the threshold.
	reg.setBroken(digestA)
	if _, err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	if len(events.Events) != 0 {
		t.Error("a single failure after a recovery raised the degraded event; the count did not " +
			"reset, so the warning would fire on any object that has ever failed")
	}
}

// An object with no repository AND no default registry has nowhere it could have published, so it
// has nothing to refresh and is not a failure.
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
		// No Default.
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

	r, _ := refresherFor(t, reg.host(), obj, secret)

	res, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if res.Refreshed != 1 {
		t.Errorf("refreshed = %d, want 1; the credential was not usable", res.Refreshed)
	}
}

// A missing Secret is a failure to refresh, not a silent skip.
func TestAnUnusableSecretIsAFailureNotASkip(t *testing.T) {
	reg := newRegistry(t)
	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{{Digest: digestA}}, nil)
	obj.Spec.Push.SecretRef = &ociv1alpha1.LocalObjectReference{Name: "absent"}

	r, _ := refresherFor(t, reg.host(), obj)

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

// refresherFor builds a Refresher over objs[0], with every obj in the fake client.
func refresherFor(t *testing.T, host string, objs ...client.Object) (*Refresher, *record.FakeRecorder) {
	t.Helper()
	events := record.NewFakeRecorder(200)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return &Refresher{
		Client:             c,
		Source:             sourceFor(objs[0], c),
		Pending:            allReconciled{},
		Recorder:           events,
		InsecureRegistries: []string{host},
	}, events
}

// drainEvents returns every event recorded so far.
func drainEvents(events *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-events.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func countReason(events []string, reason string) int {
	n := 0
	for _, ev := range events {
		if strings.Contains(ev, reason) {
			n++
		}
	}
	return n
}

func containsRef(requests []string, want string) bool {
	for _, r := range requests {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

// sourceFor picks the source for whichever kind the test built.
func sourceFor(obj client.Object, c client.Client) Source {
	if _, ok := obj.(*ociv1alpha1.ImageBuild); ok {
		return BuildSource{Client: c}
	}
	return CompositionSource{Client: c}
}

// TestEveryReferenceIsBuiltFromTheResolvedRepository pins ADR 0048 directly: every reference
// retention dials is built from the repository it resolved, never from a stored host.
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

// TestABareTagIsRecoveredFromWhateverStatusHolds pins the parse: a host may carry a colon of its own,
// and only the last one after the last slash introduces a tag.
func TestABareTagIsRecoveredFromWhateverStatusHolds(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"already bare", "v1", "v1"},
		{"public host with a port", "oci-composer.internal:30500/team-a/app:v1", "v1"},
		{"in-cluster host with a port", "registry.svc:5000/team-a/app:sb6b025064f7ba9bc", "sb6b025064f7ba9bc"},
		{"no port", "ghcr.io/example/app:v1", "v1"},
		{"empty", "", ""},
		// A repository with no tag names nothing that was published.
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

// TestTagsStoredWithAnUnresolvableHostStillRefresh is ADR 0048 end to end: tags stored under a
// public host that a pod cannot resolve must still be refreshed through the resolved repository.
func TestTagsStoredWithAnUnresolvableHostStillRefresh(t *testing.T) {
	reg := newRegistry(t)

	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestB, Tags: []string{publicTag("v0")}},
	}, &ociv1alpha1.ArtifactStatus{
		Digest: digestA,
		Ref:    publicHost + "/team/app@" + digestA,
		Tags:   []string{publicTag("v1")},
	})

	r, rec := refresherFor(t, reg.host(), obj)

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

// setBroken makes the named references answer 500 (a transient failure, not a missing manifest).
// It replaces the whole set; no arguments clears it.
func (reg *recordingRegistry) setBroken(refs ...string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.broken = map[string]bool{}
	for _, r := range refs {
		reg.broken[r] = true
	}
}

// TestAGoneReferenceDoesNotHoldTheObjectDegradedForever pins ADR 0049: a deleted history manifest
// cannot come back, so it must not keep the failure escalation armed, or a real outage would be
// indistinguishable from old noise.
func TestAGoneReferenceDoesNotHoldTheObjectDegradedForever(t *testing.T) {
	reg := newRegistry(t, digestA)
	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestA}, // gone for good
		{Digest: digestB}, // still there
	}, nil)

	r, events := refresherFor(t, reg.host(), obj)

	// Well past the point at which a failure would have escalated.
	for i := 0; i < DegradedAfter+3; i++ {
		res, err := r.RefreshOnce(context.Background())
		if err != nil {
			t.Fatalf("refreshing: %v", err)
		}
		if res.NotFound != 1 {
			t.Fatalf("notFound = %d, want 1", res.NotFound)
		}
		if res.Failed != 0 {
			t.Errorf("failed = %d, want 0: nothing here is a transient failure", res.Failed)
		}
	}

	if n := r.failures["team-a/app"]; n != 0 {
		t.Errorf("consecutiveFailures = %d after %d cycles; a permanent loss must not hold the "+
			"escalation open", n, DegradedAfter+3)
	}

	evs := drainEvents(events)
	degraded := countReason(evs, ociv1alpha1.ReasonRetentionDegraded)
	lost := countReason(evs, ociv1alpha1.ReasonRetentionLost)
	if degraded != 0 {
		t.Errorf("raised %d Degraded events; a manifest that is gone is not a refresh that is "+
			"failing", degraded)
	}
	if lost == 0 {
		t.Error("a reference was lost and nothing said so")
	}
	if !containsRef(reg.requests(), digestB) {
		t.Error("the reference that still exists stopped being refreshed")
	}
}

// recordingLogger captures what a cycle said, and at which severity.
type recordingLogger struct {
	infos  []string
	errors []string
}

func (l *recordingLogger) Info(msg string, _ ...any)           { l.infos = append(l.infos, msg) }
func (l *recordingLogger) Error(_ error, msg string, _ ...any) { l.errors = append(l.errors, msg) }

// TestASkippedCycleGetsLoud: a skipped cycle protects as little as a failed one, and one stuck
// object skips every cycle, so persistent skips must escalate like failures.
func TestASkippedCycleGetsLoud(t *testing.T) {
	r := &Refresher{
		Source:   staticSource{},
		Pending:  allReconciled{pending: []string{"team-a/stuck"}},
		Recorder: record.NewFakeRecorder(8),
	}

	log := &recordingLogger{}
	for i := 0; i < DegradedAfter-1; i++ {
		r.cycle(context.Background(), log)
	}
	if len(log.errors) != 0 {
		t.Fatalf("escalated after %d skips; a rolling restart must not read as an outage: %v",
			DegradedAfter-1, log.errors)
	}

	r.cycle(context.Background(), log)
	if len(log.errors) == 0 {
		t.Error("a refresh that has protected nothing for several cycles said so only at info, " +
			"which is how it can be off for days unnoticed")
	}

	// And it stops being loud once the view is complete again.
	r.Pending = allReconciled{}
	before := len(log.errors)
	r.cycle(context.Background(), log)
	r.cycle(context.Background(), log)
	if len(log.errors) != before {
		t.Error("the skip count did not reset once cycles ran again")
	}
}

// staticSource yields nothing; these tests are about the gate, not the refreshing.
type staticSource struct{}

func (staticSource) Targets(context.Context) ([]Target, error) { return nil, nil }

// A missing current artifact is not expired history: it raises ArtifactLost and escalates to
// Degraded if it persists (ADR 0060, amending ADR 0049; see zot#4444).
func TestALostCurrentArtifactIsLoudAndEscalates(t *testing.T) {
	reg := newRegistry(t, digestA)
	obj := buildWith(reg, "app", []ociv1alpha1.BuildRecord{
		{Digest: digestA},
		{Digest: digestB},
	}, &ociv1alpha1.ArtifactStatus{Digest: digestA})

	r, events := refresherFor(t, reg.host(), obj)

	for i := 0; i < DegradedAfter; i++ {
		if _, err := r.RefreshOnce(context.Background()); err != nil {
			t.Fatalf("refreshing: %v", err)
		}
	}

	evs := drainEvents(events)
	degraded := countReason(evs, ociv1alpha1.ReasonRetentionDegraded)
	artifactLost := countReason(evs, ociv1alpha1.ReasonArtifactLost)
	if artifactLost == 0 {
		t.Error("the artifact every workload pulls is gone and no ArtifactLost was raised; it read " +
			"exactly like history expiring")
	}
	if degraded == 0 {
		t.Errorf("the current artifact stayed gone for %d cycles without escalating", DegradedAfter)
	}
	// Still refreshing what survives.
	if !containsRef(reg.requests(), digestB) {
		t.Error("the reference that still exists stopped being refreshed")
	}
}
