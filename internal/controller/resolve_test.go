package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// tarball builds a gzipped tar and serves it, returning the URL and digest — standing in for what
// source-controller publishes.
func tarball(t *testing.T, files map[string]string) (url, digest string) {
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
	return srv.URL + "/artifact.tar.gz", "sha256:" + hex.EncodeToString(sum[:])
}

// gitRepository builds an unstructured Flux GitRepository with a published artifact.
//
// It is deliberately a source that has CAUGHT UP: generation equals observedGeneration and Ready is
// True, which is what source-controller publishes once it has fetched the revision its spec names.
// Anything less is a source whose status.artifact describes an older spec, and the resolver refuses
// to build from one (ADR 0026) — so a helper that omitted these fields would quietly be testing the
// unhappy path everywhere.
func gitRepository(name, namespace, url, digest, revision string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository",
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)
	obj.SetGeneration(1)
	_ = unstructured.SetNestedMap(obj.Object, map[string]any{
		"url": url, "digest": digest, "revision": revision,
	}, "status", "artifact")
	_ = unstructured.SetNestedField(obj.Object, int64(1), "status", "observedGeneration")
	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"type": "Ready", "status": "True", "reason": "Succeeded"},
	}, "status", "conditions")
	return obj
}

// fluxScheme registers the unstructured Flux kinds the fake client needs to serve.
func fluxScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := testScheme(t)
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository",
	}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepositoryList",
	}, &unstructured.UnstructuredList{})
	return s
}

func reconcilerWith(t *testing.T, objs ...client.Object) *ImageCompositionReconciler {
	t.Helper()
	return &ImageCompositionReconciler{
		Client:   fake.NewClientBuilder().WithScheme(fluxScheme(t)).WithObjects(objs...).Build(),
		Scheme:   fluxScheme(t),
		Recorder: record.NewFakeRecorder(16),
		Fetcher:  oci.NewFetcher(),
	}
}

func configMapLayer(name, cmName string, optional bool, target string) ociv1alpha1.Layer {
	return ociv1alpha1.Layer{
		Name:      name,
		ConfigMap: &ociv1alpha1.ConfigMapSource{Name: cmName, Optional: optional},
		To:        target,
	}
}

// TestConfigMapDigestIsResolved — the spec declares no digest, so the controller must produce one
// from the content. Without it the input hash would be blind to the ConfigMap changing.
func TestConfigMapDigestIsResolved(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "default"},
		Data:       map[string]string{"log4j.properties": "level=INFO"},
	}
	obj := composition("cm", configMapLayer("settings", "settings", false, "/config"))
	r := reconcilerWith(t, cm)

	inputs, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("resolved %d inputs, want 1", len(inputs))
	}
	if !strings.HasPrefix(inputs[0].Digest, "sha256:") {
		t.Fatalf("no digest was resolved: %q", inputs[0].Digest)
	}
	if inputs[0].Path == "" {
		t.Fatal("ConfigMap content was not materialised")
	}
}

// TestConfigMapContentChangesTheInputHash — this is the property that makes rebuilds happen. If a
// ConfigMap edit did not move the hash, the short-circuit would skip the rebuild entirely.
func TestConfigMapContentChangesTheInputHash(t *testing.T) {
	hashFor := func(value string) string {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "default"},
			Data:       map[string]string{"app.conf": value},
		}
		obj := composition("cm", configMapLayer("settings", "settings", false, "/config"))
		r := reconcilerWith(t, cm)

		inputs, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
		if err != nil {
			t.Fatalf("resolving: %v", err)
		}
		return oci.InputHash(inputs, oci.Config{}, "", nil)
	}

	// Bound to variables rather than compared inline: two identical calls in one expression read
	// as a tautology to a linter, when the point is that separate invocations agree.
	first, again := hashFor("level=INFO"), hashFor("level=INFO")
	if first == hashFor("level=DEBUG") {
		t.Fatal("changing ConfigMap content did not change the input hash")
	}
	if first != again {
		t.Fatal("identical ConfigMap content produced different hashes")
	}
}

// TestConfigMapKeyOrderDoesNotAffectTheDigest — Go map iteration is randomised, so without an
// explicit sort the same ConfigMap would hash differently between reconciles and rebuild forever.
func TestConfigMapKeyOrderDoesNotAffectTheDigest(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "many", Namespace: "default"},
		Data: map[string]string{
			"a.conf": "1", "b.conf": "2", "c.conf": "3", "d.conf": "4", "e.conf": "5",
		},
	}
	obj := composition("cm", configMapLayer("many", "many", false, "/config"))

	var first string
	for i := 0; i < 20; i++ {
		r := reconcilerWith(t, cm.DeepCopy())
		inputs, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
		if err != nil {
			t.Fatalf("resolving: %v", err)
		}
		if first == "" {
			first = inputs[0].Digest
		} else if inputs[0].Digest != first {
			t.Fatal("ConfigMap key iteration order leaked into the digest")
		}
	}
}

// TestMissingConfigMapIsPending — a non-optional ConfigMap that is absent is waited for, not
// stalled on. Creating the ConfigMap is the fix, and that bumps no generation here; ConfigMaps
// are watched, so in practice the wait ends the moment one appears.
func TestMissingConfigMapIsPending(t *testing.T) {
	obj := composition("cm", configMapLayer("settings", "absent", false, "/config"))
	r := reconcilerWith(t)

	_, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a missing non-optional ConfigMap")
	}
	var te *recon.TerminalError
	if asTerminalErr(err, &te) {
		t.Fatal("a missing ConfigMap must not be terminal; creating it bumps no generation here")
	}
	var pe *recon.PendingError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a recon.PendingError, got %T: %v", err, err)
	}
}

// TestOptionalConfigMapContributesNothing — and specifically contributes NOTHING, not an empty
// layer, which would still change the output digest.
func TestOptionalConfigMapContributesNothing(t *testing.T) {
	url, digest := tarball(t, map[string]string{"lib/a.jar": "aaa"})

	withOptional := composition("cm",
		urlLayer("core", url, digest, "/core"),
		configMapLayer("extra", "absent", true, "/config"),
	)
	without := composition("cm", urlLayer("core", url, digest, "/core"))

	r := reconcilerWith(t)

	a, _, err := r.resolveInputs(context.Background(), withOptional, t.TempDir())
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	b, _, err := r.resolveInputs(context.Background(), without, t.TempDir())
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}

	if len(a) != len(b) {
		t.Fatalf("an absent optional ConfigMap contributed %d inputs, want 0", len(a)-len(b))
	}
	if oci.InputHash(a, oci.Config{}, "", nil) != oci.InputHash(b, oci.Config{}, "", nil) {
		t.Fatal("an absent optional ConfigMap changed the input hash")
	}
}

// TestConfigMapKeyWithSeparatorIsRejected — Kubernetes should prevent it, but refusing beats
// silently placing a file somewhere unexpected in the image.
func TestConfigMapKeyWithSeparatorIsRejected(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "default"},
		Data:       map[string]string{"nested/file.conf": "x"},
	}
	obj := composition("cm", configMapLayer("bad", "bad", false, "/config"))
	r := reconcilerWith(t, cm)

	if _, _, err := r.resolveInputs(context.Background(), obj, t.TempDir()); err == nil {
		t.Fatal("accepted a ConfigMap key containing a path separator")
	}
}

// TestSourceRefDigestComesFromTheSource — resolved, not declared. The spec carries no digest and
// must not need one.
func TestSourceRefDigestComesFromTheSource(t *testing.T) {
	url, digest := tarball(t, map[string]string{"config/app.conf": "x"})
	repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

	obj := composition("git", ociv1alpha1.Layer{
		Name: "config",
		SourceRef: &ociv1alpha1.SourceRefSource{
			Kind: "GitRepository", Name: "platform-config", Subpath: "config",
		},
		To: "/config",
	})
	r := reconcilerWith(t, repo)

	inputs, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if inputs[0].Digest != digest {
		t.Fatalf("digest %q, want the source's %q", inputs[0].Digest, digest)
	}
	if inputs[0].URL != url {
		t.Fatalf("URL %q, want the source's %q", inputs[0].URL, url)
	}
	if inputs[0].Subpath != "config" {
		t.Fatalf("subpath %q, want %q", inputs[0].Subpath, "config")
	}
	// source-controller always publishes a gzipped tar, whatever the source kind.
	if inputs[0].Unpack != oci.UnpackTarGz {
		t.Fatalf("unpack %q, want tar.gz", inputs[0].Unpack)
	}
}

// TestSourceRefDefaultsToTheObjectNamespace — the common case is a source in the same namespace.
func TestSourceRefDefaultsToTheObjectNamespace(t *testing.T) {
	url, digest := tarball(t, map[string]string{"a": "1"})
	repo := gitRepository("local", "default", url, digest, "main@sha1:abcd")

	obj := composition("git", ociv1alpha1.Layer{
		Name:      "content",
		SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "local"},
		To:        "/content",
	})
	r := reconcilerWith(t, repo)

	if _, _, err := r.resolveInputs(context.Background(), obj, t.TempDir()); err != nil {
		t.Fatalf("resolving: %v", err)
	}
}

// TestMissingSourceIsPendingNotTerminal — a composition and its GitRepository applied in ONE
// commit race, and the loser used to stall permanently while the source it needed sat there
// Ready. Applying both together is the normal case, so this must converge on its own.
//
// Stalling is only safe when editing this object's spec is the fix, because the generation change
// is the wake-up. Creating the source raises no event here, so a stall here waits forever.
func TestMissingSourceIsPendingNotTerminal(t *testing.T) {
	obj := composition("git", ociv1alpha1.Layer{
		Name:      "content",
		SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "absent"},
		To:        "/content",
	})
	r := reconcilerWith(t)

	_, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a source that does not exist")
	}
	var te *recon.TerminalError
	if asTerminalErr(err, &te) {
		t.Fatal("a source that does not exist YET must not be terminal; creating it bumps no generation here")
	}
	var pe *recon.PendingError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a recon.PendingError, got %T: %v", err, err)
	}
}

// TestSourceWithoutAnArtifactIsTransient — source-controller may simply not have reconciled yet,
// which resolves itself. Stalling would need a human to intervene for a race.
func TestSourceWithoutAnArtifactIsTransient(t *testing.T) {
	repo := &unstructured.Unstructured{}
	repo.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository",
	})
	repo.SetName("fresh")
	repo.SetNamespace("default")

	obj := composition("git", ociv1alpha1.Layer{
		Name:      "content",
		SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "fresh"},
		To:        "/content",
	})
	r := reconcilerWith(t, repo)

	_, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a source with no artifact")
	}
	var te *recon.TerminalError
	if asTerminalErr(err, &te) {
		t.Fatal("a source that has not published yet must be transient, not Stalled")
	}
}

// TestLayerOrderIsPreservedAcrossSourceKinds — layers are contributed in declaration order, and
// mixing source kinds must not reorder anything. See ADR 0003.
func TestLayerOrderIsPreservedAcrossSourceKinds(t *testing.T) {
	url, digest := tarball(t, map[string]string{"lib/a.jar": "aaa"})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "default"},
		Data:       map[string]string{"app.conf": "x"},
	}
	repo := gitRepository("repo", "default", url, digest, "main@sha1:abcd")

	obj := composition("mixed",
		configMapLayer("first", "settings", false, "/a"),
		urlLayer("second", url, digest, "/b"),
		ociv1alpha1.Layer{
			Name:      "third",
			SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "repo"},
			To:        "/c",
		},
	)
	r := reconcilerWith(t, cm, repo)

	inputs, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	want := []string{"first", "second", "third"}
	if len(inputs) != len(want) {
		t.Fatalf("resolved %d inputs, want %d", len(inputs), len(want))
	}
	for i, name := range want {
		if inputs[i].Name != name {
			t.Fatalf("input %d is %q, want %q — declaration order was not preserved",
				i, inputs[i].Name, name)
		}
	}
}

// asTerminalErr is errors.As, named so the intent reads at each call site.
func asTerminalErr(err error, target **recon.TerminalError) bool { return errors.As(err, target) }

// TestSourceRefRefusesAnotherNamespace — the controller's RBAC over Flux sources is cluster-wide,
// so without this a tenant who can create an ImageComposition could name any namespace's source and
// bake its content into an image they control and can read. It is the one tenancy boundary a spec
// could otherwise cross on its own.
//
// Terminal rather than pending: editing this spec is what fixes it.
func TestSourceRefRefusesAnotherNamespace(t *testing.T) {
	url, digest := tarball(t, map[string]string{"secrets/app.conf": "not yours"})
	repo := gitRepository("platform-config", "other-team", url, digest, "main@sha1:abcd")

	obj := composition("cross", ociv1alpha1.Layer{
		Name: "config",
		SourceRef: &ociv1alpha1.SourceRefSource{
			Kind: "GitRepository", Name: "platform-config", Namespace: "other-team",
		},
		To: "/config",
	})
	r := reconcilerWith(t, repo)

	inputs, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err == nil {
		t.Fatalf("another namespace's source resolved: %+v", inputs)
	}
	if !recon.IsTerminal(err) {
		t.Errorf("error is not terminal, so it would be retried forever: %v", err)
	}
	if !strings.Contains(err.Error(), "same namespace") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// The same namespace stated explicitly must still work — the field is not banned, only a value
// pointing somewhere else.
func TestSourceRefAllowsItsOwnNamespaceStatedExplicitly(t *testing.T) {
	url, digest := tarball(t, map[string]string{"config/app.conf": "x"})
	repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

	obj := composition("explicit", ociv1alpha1.Layer{
		Name: "config",
		SourceRef: &ociv1alpha1.SourceRefSource{
			Kind: "GitRepository", Name: "platform-config", Namespace: "default",
		},
		To: "/config",
	})
	r := reconcilerWith(t, repo)

	if _, _, err := r.resolveInputs(context.Background(), obj, t.TempDir()); err != nil {
		t.Fatalf("a source in the object's own namespace was refused: %v", err)
	}
}

// TestSourceRefHashesTheRevisionNotTheTarball — source-controller re-packs artifacts on restart, so
// the tarball's digest moves while the revision it describes does not. Hashing the digest rebuilt
// every composition consuming that source for bytes that were identical.
func TestSourceRefHashesTheRevisionNotTheTarball(t *testing.T) {
	url, digest := tarball(t, map[string]string{"config/app.conf": "x"})
	repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

	obj := composition("identity", ociv1alpha1.Layer{
		Name:      "config",
		SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "platform-config"},
		To:        "/config",
	})
	r := reconcilerWith(t, repo)

	inputs, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if inputs[0].Identity != "main@sha1:abcd" {
		t.Errorf("identity = %q, want the revision; a repack would otherwise rebuild everything",
			inputs[0].Identity)
	}
	// The digest still has to be what the fetch is verified against.
	if inputs[0].Digest != digest {
		t.Errorf("digest = %q, want the artifact's %q", inputs[0].Digest, digest)
	}
}

// TestHistoryRecordsWhereEachLayerCameFrom — the gap that made ADR 0026's incident expensive.
//
// A wrong artifact was published and the only way to find out was to pull the manifest, fetch the
// layer and read its payload, because nothing linked the digest to what produced it. This proves
// the record reaches history; TestSourceRefHashesTheRevisionNotTheTarball proves a Flux source
// contributes its revision to it.
//
// A fetch layer records no revision, and that is correct rather than missing: its declared digest
// IS its identity, so there is nothing else to name.
func TestHistoryRecordsWhereEachLayerCameFrom(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("provenance", urlLayer("core", url, digest, "/core"))
	r, _ := servingReconciler(t, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := reload(t, r, obj)
	if len(got.Status.History) == 0 {
		t.Fatal("no build recorded")
	}
	sources := got.Status.History[0].Sources
	if len(sources) != 1 {
		t.Fatalf("recorded %d sources, want 1: %+v", len(sources), sources)
	}
	if sources[0].Name != "core" {
		t.Errorf("source name = %q, want %q", sources[0].Name, "core")
	}
	if sources[0].Digest != digest {
		t.Errorf("digest = %q, want the layer's %q; without it an artifact cannot be traced back "+
			"to what produced it", sources[0].Digest, digest)
	}
}
