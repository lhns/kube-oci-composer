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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	"github.com/lhns/kube-oci-composer/internal/serve"
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
func servingReconciler(t *testing.T, objs ...*ociv1alpha1.ImageComposition) (*ImageCompositionReconciler, string) {
	t.Helper()

	srv, err := serve.New("oci.test", ":0", t.TempDir())
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// httptest picks the port, so the Server must be told which one it actually got — the
	// controller pushes to its own endpoint over loopback.
	host := strings.TrimPrefix(httpSrv.URL, "http://")
	srv.Addr = host[strings.LastIndex(host, ":"):]

	builder := fake.NewClientBuilder().WithScheme(testScheme(t))
	for _, o := range objs {
		builder = builder.WithObjects(o).WithStatusSubresource(o)
	}

	return &ImageCompositionReconciler{
		Client:   builder.Build(),
		Scheme:   testScheme(t),
		Recorder: record.NewFakeRecorder(64),
		Server:   srv,
		Fetcher:  oci.NewFetcher(),
	}, host
}

func composition(name string, layers ...ociv1alpha1.Layer) *ociv1alpha1.ImageComposition {
	return &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ociv1alpha1.ImageCompositionSpec{
			Interval: metav1.Duration{Duration: 0},
			Layers:   layers,
			Publish:  &ociv1alpha1.Publish{Name: name, Tag: "main"},
		},
	}
}

func urlLayer(name, url, digest, target string) ociv1alpha1.Layer {
	return ociv1alpha1.Layer{
		Name:      name,
		URLSource: &ociv1alpha1.URLSource{URL: url},
		Digest:    digest,
		Unpack:    ociv1alpha1.UnpackTarGz,
		Target:    target,
	}
}

// TestServingModePublishesAndIsIdempotent covers the central claim: with no registry configured
// the controller publishes to its own endpoint, and reconciling again converges without
// republishing.
func TestServingModePublishesAndIsIdempotent(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("plugins", urlLayer("core", url, digest, "/core"))
	r, host := servingReconciler(t, obj)

	art, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if art.Digest == "" {
		t.Fatal("no digest recorded")
	}

	// status must carry the PULL host, not the loopback address the controller wrote to.
	if !strings.HasPrefix(art.Ref, "oci.test/plugins:main@") {
		t.Fatalf("ref %q does not use the serving host", art.Ref)
	}
	wantContent := "oci.test/plugins:main-" + strings.TrimPrefix(art.Digest, "sha256:")[:12]
	if art.ContentTag != wantContent {
		t.Fatalf("content tag %q, want %q", art.ContentTag, wantContent)
	}

	// Both references must actually resolve, and to the same manifest.
	for _, ref := range []string{
		fmt.Sprintf("%s/plugins:main", host),
		fmt.Sprintf("%s/plugins:main-%s", host, strings.TrimPrefix(art.Digest, "sha256:")[:12]),
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

	second, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
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
	r, host := servingReconciler(t, obj)

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil {
		t.Fatal("expected an error for a mismatched digest")
	}

	var te *terminalError
	if !errors.As(err, &te) {
		t.Fatalf("digest mismatch must be terminal, got %T: %v", err, err)
	}
	if got := reasonFor(err); got != ociv1alpha1.ReasonDigestMismatch {
		t.Fatalf("reason %q, want %q", got, ociv1alpha1.ReasonDigestMismatch)
	}

	ref, err := name.ParseReference(host+"/tampered:main", name.Insecure)
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
	r, _ := servingReconciler(t, obj)

	first, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("reconcile A: %v", err)
	}

	obj.Spec.Layers[0] = urlLayer("core", urlB, digestB, "/core")
	changed, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("reconcile B: %v", err)
	}
	if changed.Digest == first.Digest {
		t.Fatal("changing the content did not change the digest")
	}

	obj.Spec.Layers[0] = urlLayer("core", urlA, digestA, "/core")
	reverted, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("reconcile A again: %v", err)
	}
	if reverted.Digest != first.Digest {
		t.Fatalf("reverting gave %s, want the original %s", reverted.Digest, first.Digest)
	}
}

// TestOldContentTagSurvivesRebuild is the point of dual publication: the moving pointer moves,
// but a previously published content tag keeps resolving to its original bytes. Anything that
// pinned it stays pinned.
func TestOldContentTagSurvivesRebuild(t *testing.T) {
	urlA, digestA := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	urlB, digestB := contentServer(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("dual", urlLayer("core", urlA, digestA, "/core"))
	r, host := servingReconciler(t, obj)

	first, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	oldContentTag := fmt.Sprintf("%s/dual:main-%s", host, strings.TrimPrefix(first.Digest, "sha256:")[:12])

	obj.Spec.Layers[0] = urlLayer("core", urlB, digestB, "/core")
	second, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Digest == first.Digest {
		t.Fatal("rebuild produced the same digest")
	}

	ref, err := name.ParseReference(oldContentTag, name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	desc, err := remote.Head(ref)
	if err != nil {
		t.Fatalf("old content tag no longer resolves: %v", err)
	}
	if desc.Digest.String() != first.Digest {
		t.Fatalf("old content tag now resolves to %s, want %s", desc.Digest, first.Digest)
	}

	moving, err := name.ParseReference(host+"/dual:main", name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	movingDesc, err := remote.Head(moving)
	if err != nil {
		t.Fatalf("moving pointer: %v", err)
	}
	if movingDesc.Digest.String() != second.Digest {
		t.Fatalf("moving pointer resolves to %s, want the newest %s", movingDesc.Digest, second.Digest)
	}
}

// TestTagListingWorks — ImageRepository scanning depends on /v2/<name>/tags/list, which is the
// integration point most likely to be subtly wrong.
func TestTagListingWorks(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("listing", urlLayer("core", url, digest, "/core"))
	r, host := servingReconciler(t, obj)

	art, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	repo, err := name.NewRepository(host+"/listing", name.Insecure)
	if err != nil {
		t.Fatalf("parsing repo: %v", err)
	}
	tags, err := remote.List(repo)
	if err != nil {
		t.Fatalf("listing tags: %v", err)
	}

	want := map[string]bool{
		"main": false,
		"main-" + strings.TrimPrefix(art.Digest, "sha256:")[:12]: false,
	}
	for _, tag := range tags {
		if _, ok := want[tag]; ok {
			want[tag] = true
		}
	}
	for tag, seen := range want {
		if !seen {
			t.Fatalf("tag %q missing from listing %v", tag, tags)
		}
	}
}

// TestServingModeWithoutServerIsTerminal — misconfiguration must say so plainly rather than
// retry forever against an endpoint that does not exist.
func TestServingModeWithoutServerIsTerminal(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("noserver", urlLayer("core", url, digest, "/core"))

	r := &ImageCompositionReconciler{
		Client:   fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme:   testScheme(t),
		Recorder: record.NewFakeRecorder(8),
		Fetcher:  oci.NewFetcher(),
	}

	_, err := r.reconcileArtifact(context.Background(), obj)
	var te *terminalError
	if err == nil || !errors.As(err, &te) {
		t.Fatalf("expected a terminal error, got %v", err)
	}
}
