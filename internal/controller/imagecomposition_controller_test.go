package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := ociv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding api scheme: %v", err)
	}
	return s
}

// contentServer serves a gzipped tar over HTTP and returns its URL and sha256, standing in for
// a GitHub release asset.
func contentServer(t *testing.T, files map[string]string) (url, digest string) {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	payload := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	sum := sha256.Sum256(payload)
	return srv.URL + "/content.tar.gz", "sha256:" + hex.EncodeToString(sum[:])
}

// servingReconciler wires a reconciler to an in-process OCI endpoint, i.e. exactly the default
// no-registry mode the design promises.
// registryReconciler stands up an in-memory registry and points the controller's default at it.
//
// It was servingReconciler, wiring an internal/serve endpoint the controller pushed to over
// loopback. That endpoint is gone (ADR 0035), and so is the only reason these tests differed from
// the path production takes: they now push to a registry, because that is the only thing the
// controller does.
//
// go-containerregistry's own in-memory registry rather than a hand-rolled stub -- it enforces the
// distribution spec, so a manifest this controller writes and cannot read back is a real defect
// rather than an artefact of a lenient double.
func registryReconciler(t *testing.T, objs ...*ociv1alpha1.ImageComposition) (*ImageCompositionReconciler, string) {
	t.Helper()

	httpSrv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(httpSrv.Close)
	host := strings.TrimPrefix(httpSrv.URL, "http://")

	builder := fake.NewClientBuilder().WithScheme(testScheme(t))
	for _, o := range objs {
		builder = builder.WithObjects(o).WithStatusSubresource(o)
	}

	return &ImageCompositionReconciler{
		Client:   builder.Build(),
		Scheme:   testScheme(t),
		Recorder: record.NewFakeRecorder(64),
		Default:  recon.DefaultRegistry{Host: host},
		Fetcher:  oci.NewFetcher(),
	}, host
}

func composition(name string, layers ...ociv1alpha1.Layer) *ociv1alpha1.ImageComposition {
	return &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ociv1alpha1.ImageCompositionSpec{
			Layers: layers,
			Push:   &ociv1alpha1.Push{Tags: []string{"main"}, Immutable: ptr.To(false)},
		},
	}
}

// build runs one build and writes the result back into obj.Status the way Reconcile does.
// Threading the status through matters: it is what lets the inputHash short-circuit engage on a
// second call, so tests exercise the same path production takes rather than a simplified one.
func build(t *testing.T, r *ImageCompositionReconciler, obj *ociv1alpha1.ImageComposition, what string) *ociv1alpha1.ArtifactStatus {
	t.Helper()
	res, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	obj.Status.Artifact = res.Artifact
	obj.Status.InputHash = res.InputHash
	obj.Status.History = recon.RecordHistory(obj.Status.History, res.Record, r.historyLimit(obj))
	return res.Artifact
}

func urlLayer(name, url, digest, to string) ociv1alpha1.Layer {
	return ociv1alpha1.Layer{
		Name: name,
		Fetch: &ociv1alpha1.FetchSource{
			URL: url, Digest: digest, Unpack: ociv1alpha1.UnpackTarGz,
		},
		To: to,
	}
}

// TestServingModePublishesAndIsIdempotent covers the central claim: with no registry configured
// the controller publishes to its own endpoint, and reconciling again converges without
// republishing.
func TestPublishingIsIdempotent(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("plugins", urlLayer("core", url, digest, "/core"))
	r, host := registryReconciler(t, obj)

	art := build(t, r, obj, "first reconcile")
	if art.Digest == "" {
		t.Fatal("no digest recorded")
	}

	// status carries the reference a consumer pulls: the registry, namespace-qualified.
	if !strings.HasPrefix(art.Ref, host+"/default/plugins:main@") {
		t.Fatalf("ref %q does not name the registry it was published to", art.Ref)
	}
	if want := []string{host + "/default/plugins:main"}; !slices.Equal(art.Tags, want) {
		t.Fatalf("tags %v, want %v", art.Tags, want)
	}

	// Both the tag and the digest must resolve, and to the same manifest. The digest reference
	// is the one a build with no tags would have to rely on entirely.
	for _, ref := range []string{
		fmt.Sprintf("%s/default/plugins:main", host),
		fmt.Sprintf("%s/default/plugins@%s", host, art.Digest),
	} {
		parsed, err := name.ParseReference(ref, name.Insecure)
		if err != nil {
			t.Fatalf("parsing %s: %v", ref, err)
		}
		desc, err := remote.Head(parsed)
		if err != nil {
			t.Fatalf("resolving %s: %v", ref, err)
		}
		if desc.Digest.String() != art.Digest {
			t.Fatalf("%s resolves to %s, want %s", ref, desc.Digest, art.Digest)
		}
	}

	second := build(t, r, obj, "second reconcile")
	if second.Digest != art.Digest {
		t.Fatalf("digest changed on re-reconcile: %s then %s", art.Digest, second.Digest)
	}
}

// TestDigestMismatchIsTerminalAndPublishesNothing is the tamper-evidence property. A wrong
// digest must stall rather than retry, and must not publish.
func TestDigestMismatchIsTerminalAndPublishesNothing(t *testing.T) {
	url, _ := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	wrong := "sha256:" + strings.Repeat("0", 64)
	obj := composition("tampered", urlLayer("core", url, wrong, "/core"))
	r, host := registryReconciler(t, obj)

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil {
		t.Fatal("expected an error for a mismatched digest")
	}

	var te *recon.TerminalError
	if !errors.As(err, &te) {
		t.Fatalf("digest mismatch must be terminal, got %T: %v", err, err)
	}
	if got := reasonFor(err); got != ociv1alpha1.ReasonDigestMismatch {
		t.Fatalf("reason %q, want %q", got, ociv1alpha1.ReasonDigestMismatch)
	}

	ref, err := name.ParseReference(host+"/default/tampered:main", name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := remote.Head(ref); err == nil {
		t.Fatal("something was published despite the digest mismatch")
	}
}

// TestConvergenceOnChangedContent — changing an input must produce a new digest, and reverting
// must return to the original one. That round trip is what makes the design deterministic
// rather than merely repeatable.
func TestConvergenceOnChangedContent(t *testing.T) {
	urlA, digestA := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	urlB, digestB := contentServer(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("converge", urlLayer("core", urlA, digestA, "/core"))
	r, _ := registryReconciler(t, obj)

	first := build(t, r, obj, "reconcile A")

	obj.Spec.Layers[0] = urlLayer("core", urlB, digestB, "/core")
	changed := build(t, r, obj, "reconcile B")
	if changed.Digest == first.Digest {
		t.Fatal("changing the content did not change the digest")
	}

	obj.Spec.Layers[0] = urlLayer("core", urlA, digestA, "/core")
	reverted := build(t, r, obj, "reconcile A again")
	if reverted.Digest != first.Digest {
		t.Fatalf("reverting gave %s, want the original %s", reverted.Digest, first.Digest)
	}
}

// TestOldDigestSurvivesRebuild — a workload pinned to a digest must keep resolving after a
// rebuild has moved the tag on. This is what the dropped content tag used to provide, and the
// digest reference provides it directly.
func TestOldDigestSurvivesRebuild(t *testing.T) {
	urlA, digestA := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	urlB, digestB := contentServer(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("dual", urlLayer("core", urlA, digestA, "/core"))
	r, host := registryReconciler(t, obj)

	first := build(t, r, obj, "first")

	obj.Spec.Layers[0] = urlLayer("core", urlB, digestB, "/core")
	second := build(t, r, obj, "second")
	if second.Digest == first.Digest {
		t.Fatal("rebuild produced the same digest")
	}

	resolves := func(ref, want string) {
		t.Helper()
		parsed, err := name.ParseReference(ref, name.Insecure)
		if err != nil {
			t.Fatalf("parsing %s: %v", ref, err)
		}
		desc, err := remote.Head(parsed)
		if err != nil {
			t.Fatalf("%s no longer resolves: %v", ref, err)
		}
		if desc.Digest.String() != want {
			t.Fatalf("%s resolves to %s, want %s", ref, desc.Digest, want)
		}
	}

	resolves(fmt.Sprintf("%s/default/dual@%s", host, first.Digest), first.Digest)
	resolves(fmt.Sprintf("%s/default/dual@%s", host, second.Digest), second.Digest)
	// This object opts out of immutability, so its tag is a pointer and follows the newest build.
	resolves(host+"/default/dual:main", second.Digest)
}

// TestSpecHashTagPattern is the pattern from ADR 0017 end to end: a tag derived from the spec,
// with immutability left at its default. Changing the spec changes the tag, so both keep
// resolving to their own content and neither is ever remeaned — which is what makes it safe for
// a workload to reference the tag rather than a digest.
func TestSpecHashTagPattern(t *testing.T) {
	urlA, digestA := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	urlB, digestB := contentServer(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("hashed", urlLayer("core", urlA, digestA, "/core"))
	obj.Spec.Push = &ociv1alpha1.Push{Tags: []string{"sAAAA"}}
	r, host := registryReconciler(t, obj)

	first := build(t, r, obj, "first")

	// A different spec is a different tag, exactly as the consumer would have computed.
	obj.Spec.Layers[0] = urlLayer("core", urlB, digestB, "/core")
	obj.Spec.Push.Tags = []string{"sBBBB"}
	second := build(t, r, obj, "second")
	if second.Digest == first.Digest {
		t.Fatal("rebuild produced the same digest")
	}

	for ref, want := range map[string]string{
		host + "/default/hashed:sAAAA": first.Digest,
		host + "/default/hashed:sBBBB": second.Digest,
	} {
		parsed, err := name.ParseReference(ref, name.Insecure)
		if err != nil {
			t.Fatalf("parsing %s: %v", ref, err)
		}
		desc, err := remote.Head(parsed)
		if err != nil {
			t.Fatalf("%s does not resolve: %v", ref, err)
		}
		if desc.Digest.String() != want {
			t.Fatalf("%s resolves to %s, want %s", ref, desc.Digest, want)
		}
	}
}

// TestImmutableTagRefusesToBeRemeaned — the guard that makes referencing a tag safe. Reusing a
// tag for different content must fail terminally rather than silently leaving nodes on stale
// bytes under an unchanged name.
func TestImmutableTagRefusesToBeRemeaned(t *testing.T) {
	urlA, digestA := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	urlB, digestB := contentServer(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("pinned", urlLayer("core", urlA, digestA, "/core"))
	obj.Spec.Push = &ociv1alpha1.Push{Tags: []string{"v1"}}
	r, _ := registryReconciler(t, obj)

	build(t, r, obj, "first")

	// Same tag, different content: the case the guard exists for.
	obj.Spec.Layers[0] = urlLayer("core", urlB, digestB, "/core")
	if _, err := r.reconcileArtifact(t.Context(), obj); err == nil {
		t.Fatal("remeaning an immutable tag was allowed")
	} else if reasonFor(err) != ociv1alpha1.ReasonImmutableConflict {
		t.Fatalf("reason %q, want %q", reasonFor(err), ociv1alpha1.ReasonImmutableConflict)
	}

	// Opting out is what a genuinely moving pointer does.
	obj.Spec.Push.Immutable = ptr.To(false)
	if art := build(t, r, obj, "after opting out"); art.Digest == "" {
		t.Fatal("immutable: false did not allow the tag to move")
	}
}

// TestDigestOnlyPublishing — no tags at all is a supported mode, for anyone pinning digests via
// image automation. The content must still be pullable.
func TestDigestOnlyPublishing(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("untagged", urlLayer("core", url, digest, "/core"))
	obj.Spec.Push = &ociv1alpha1.Push{}
	r, host := registryReconciler(t, obj)

	art := build(t, r, obj, "first")
	if len(art.Tags) != 0 {
		t.Fatalf("tags %v, want none", art.Tags)
	}
	if art.Ref != fmt.Sprintf("%s/default/untagged@%s", host, art.Digest) {
		t.Fatalf("ref %q is not a bare digest reference", art.Ref)
	}

	ref, err := name.ParseReference(fmt.Sprintf("%s/default/untagged@%s", host, art.Digest), name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := remote.Head(ref); err != nil {
		t.Fatalf("digest-only artifact is not pullable: %v", err)
	}

	// And it must still converge rather than rebuilding forever with nothing to HEAD by tag.
	if second := build(t, r, obj, "second"); second.Digest != art.Digest {
		t.Fatalf("second reconcile produced %s, want %s", second.Digest, art.Digest)
	}
}

// TestTagListingWorks — ImageRepository scanning depends on /v2/<name>/tags/list, which is the
// integration point most likely to be subtly wrong.
func TestTagListingWorks(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("listing", urlLayer("core", url, digest, "/core"))
	r, host := registryReconciler(t, obj)

	art := build(t, r, obj, "reconcile")

	repo, err := name.NewRepository(host+"/default/listing", name.Insecure)
	if err != nil {
		t.Fatalf("parsing repo: %v", err)
	}
	tags, err := remote.List(repo)
	if err != nil {
		t.Fatalf("listing tags: %v", err)
	}

	// Exactly the tags that were asked for, and nothing invented alongside them. The listing is
	// what a scanner sees, so a stray derived tag would show up as a candidate release.
	if !slices.Contains(tags, "main") {
		t.Fatalf(`tag "main" missing from listing %v`, tags)
	}
	for _, tag := range tags {
		if tag != "main" {
			t.Fatalf("unexpected extra tag %q in listing %v", tag, tags)
		}
	}
	if art.Digest == "" {
		t.Fatal("nothing was published, so the listing proves nothing")
	}
}

// TestServingModeWithoutServerIsTerminal — misconfiguration must say so plainly rather than
// retry forever against an endpoint that does not exist.
func TestServingModeWithoutServerIsPending(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("noserver", urlLayer("core", url, digest, "/core"))

	r := &ImageCompositionReconciler{
		Client:   fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme:   testScheme(t),
		Recorder: record.NewFakeRecorder(8),
		Fetcher:  oci.NewFetcher(),
	}

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil {
		t.Fatal("expected an error with no serving endpoint and no spec.push")
	}
	// Pending, not terminal: giving the operator an endpoint means restarting it with different
	// flags, which changes nothing about any ImageComposition. Stalling would leave every
	// composition in the cluster wedged after the fix landed.
	var te *recon.TerminalError
	if errors.As(err, &te) {
		t.Fatal("operator misconfiguration must not stall the object")
	}
	var pe *recon.PendingError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a recon.PendingError, got %T: %v", err, err)
	}
}
