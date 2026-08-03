//go:build integration

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

func integrationScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := ociv1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// apply creates an object and returns the API server's error, if any. The rules under test are
// CEL, which only a real API server evaluates — a fake client accepts everything.
func apply(t *testing.T, name string, spec ociv1alpha1.ImageCompositionSpec) error {
	t.Helper()
	obj := &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
	err := k8s.Create(integrationCtx(t), obj)
	if err == nil {
		t.Cleanup(func() { _ = k8s.Delete(integrationCtx(t), obj) })
	}
	return err
}

func urlLayerSpec(name, url, digest string) ociv1alpha1.Layer {
	return ociv1alpha1.Layer{
		Name:      name,
		URLSource: &ociv1alpha1.URLSource{URL: url},
		Digest:    digest,
		Target:    "/x",
	}
}

const validDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// TestIntegrationValidSpecIsAccepted anchors the negative cases below: without it, a rule that
// rejected everything would look like a complete pass.
func TestIntegrationValidSpecIsAccepted(t *testing.T) {
	err := apply(t, "valid", ociv1alpha1.ImageCompositionSpec{
		Layers:  []ociv1alpha1.Layer{urlLayerSpec("core", "https://example.com/a.tgz", validDigest)},
		Publish: &ociv1alpha1.Publish{Name: "valid", Tag: "main"},
	})
	if err != nil {
		t.Fatalf("a valid spec was rejected: %v", err)
	}
}

// TestIntegrationLayerUnionIsEnforced — exactly one source. This is the rule that keeps the
// discriminated union honest, and nothing else checks it.
func TestIntegrationLayerUnionIsEnforced(t *testing.T) {
	t.Run("no source", func(t *testing.T) {
		err := apply(t, "no-source", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{Name: "empty", Target: "/x"}},
		})
		if err == nil {
			t.Fatal("a layer with no source was accepted")
		}
		if !strings.Contains(err.Error(), "exactly one of") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("two sources", func(t *testing.T) {
		err := apply(t, "two-sources", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:         "both",
				URLSource:    &ociv1alpha1.URLSource{URL: "https://example.com/a.tgz"},
				ConfigMapRef: &ociv1alpha1.ConfigMapRef{Name: "cm"},
				Digest:       validDigest,
				Target:       "/x",
			}},
		})
		if err == nil {
			t.Fatal("a layer with two sources was accepted")
		}
	})
}

// TestIntegrationDigestRuleMatchesTheSourceKind — declared for url, resolved for the rest. A url
// without a digest would break the guarantee in ADR 0002; a digest on a configMapRef would be
// ignored, which is worse than being refused because it looks like it is doing something.
func TestIntegrationDigestRuleMatchesTheSourceKind(t *testing.T) {
	t.Run("url without a digest is rejected", func(t *testing.T) {
		err := apply(t, "url-no-digest", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:      "core",
				URLSource: &ociv1alpha1.URLSource{URL: "https://example.com/a.tgz"},
				Target:    "/x",
			}},
		})
		if err == nil {
			t.Fatal("a url layer without a digest was accepted")
		}
	})

	t.Run("configMapRef with a digest is rejected", func(t *testing.T) {
		err := apply(t, "cm-with-digest", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:         "settings",
				ConfigMapRef: &ociv1alpha1.ConfigMapRef{Name: "cm"},
				Digest:       validDigest,
				Target:       "/x",
			}},
		})
		if err == nil {
			t.Fatal("a configMapRef layer with a declared digest was accepted")
		}
	})

	t.Run("configMapRef without a digest is accepted", func(t *testing.T) {
		err := apply(t, "cm-no-digest", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:         "settings",
				ConfigMapRef: &ociv1alpha1.ConfigMapRef{Name: "cm"},
				Target:       "/x",
			}},
		})
		if err != nil {
			t.Fatalf("a valid configMapRef layer was rejected: %v", err)
		}
	})

	t.Run("sourceRef without a digest is accepted", func(t *testing.T) {
		err := apply(t, "src-no-digest", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:      "config",
				SourceRef: &ociv1alpha1.SourceRef{Kind: "GitRepository", Name: "repo"},
				Target:    "/x",
			}},
		})
		if err != nil {
			t.Fatalf("a valid sourceRef layer was rejected: %v", err)
		}
	})
}

// TestIntegrationImageSourceIsAccepted — a base image plus content on top, which is the shape
// that produces a runnable image rather than a bundle to mount.
func TestIntegrationImageSourceIsAccepted(t *testing.T) {
	err := apply(t, "image-source", ociv1alpha1.ImageCompositionSpec{
		Layers: []ociv1alpha1.Layer{
			{
				Name:   "base",
				Image:  &ociv1alpha1.ImageSource{Repository: "gcr.io/distroless/static"},
				Digest: validDigest,
				Target: "/",
			},
			urlLayerSpec("plugins", "https://example.com/plugins.tgz", validDigest),
		},
		Config:  &ociv1alpha1.ImageConfig{From: "base"},
		Publish: &ociv1alpha1.Publish{Name: "image-source", Tag: "main"},
	})
	if err != nil {
		t.Fatalf("a base-image composition was rejected: %v", err)
	}
}

// TestIntegrationImageSourceStillNeedsADigest — an image entry is content-addressed like every
// other source. A tag here would make the output depend on when it was built.
func TestIntegrationImageSourceStillNeedsADigest(t *testing.T) {
	err := apply(t, "image-no-digest", ociv1alpha1.ImageCompositionSpec{
		Layers: []ociv1alpha1.Layer{{
			Name:  "base",
			Image: &ociv1alpha1.ImageSource{Repository: "gcr.io/distroless/static"},
		}},
	})
	if err == nil {
		t.Fatal("an image layer without a digest was accepted")
	}
}

// TestIntegrationPushAndPublishAreMutuallyExclusive — setting both would leave it ambiguous where
// the artifact goes and what status.artifact.ref should say.
func TestIntegrationPushAndPublishAreMutuallyExclusive(t *testing.T) {
	err := apply(t, "both-targets", ociv1alpha1.ImageCompositionSpec{
		Layers:  []ociv1alpha1.Layer{urlLayerSpec("core", "https://example.com/a.tgz", validDigest)},
		Publish: &ociv1alpha1.Publish{Name: "x", Tag: "main"},
		Push:    &ociv1alpha1.Push{Repository: "ghcr.io/example/x", Tag: "main"},
	})
	if err == nil {
		t.Fatal("both push and publish were accepted")
	}
}

// TestIntegrationEmptyLayersIsRejected — an empty list is far more likely to be a templating
// accident than a deliberate empty artifact. See ADR 0003.
func TestIntegrationEmptyLayersIsRejected(t *testing.T) {
	if err := apply(t, "no-layers", ociv1alpha1.ImageCompositionSpec{
		Publish: &ociv1alpha1.Publish{Name: "x", Tag: "main"},
	}); err == nil {
		t.Fatal("an ImageComposition with no layers was accepted")
	}
}

// TestIntegrationMalformedValuesAreRejected — the field-level patterns.
func TestIntegrationMalformedValuesAreRejected(t *testing.T) {
	cases := map[string]ociv1alpha1.Layer{
		"non-sha256 digest": {
			Name: "x", URLSource: &ociv1alpha1.URLSource{URL: "https://example.com/a.tgz"},
			Digest: "md5:abcd", Target: "/x",
		},
		"short digest": {
			Name: "x", URLSource: &ociv1alpha1.URLSource{URL: "https://example.com/a.tgz"},
			Digest: "sha256:abcd", Target: "/x",
		},
		"non-http url": {
			Name: "x", URLSource: &ociv1alpha1.URLSource{URL: "ftp://example.com/a.tgz"},
			Digest: validDigest, Target: "/x",
		},
		"relative target": {
			Name: "x", URLSource: &ociv1alpha1.URLSource{URL: "https://example.com/a.tgz"},
			Digest: validDigest, Target: "relative/path",
		},
		"unknown unpack mode": {
			Name: "x", URLSource: &ociv1alpha1.URLSource{URL: "https://example.com/a.tgz"},
			Digest: validDigest, Target: "/x", Unpack: "zip",
		},
		"unknown source kind": {
			Name: "x", SourceRef: &ociv1alpha1.SourceRef{Kind: "HelmRepository", Name: "r"},
			Target: "/x",
		},
	}

	for name, layer := range cases {
		t.Run(name, func(t *testing.T) {
			err := apply(t, "malformed-"+strings.ReplaceAll(name, " ", "-"),
				ociv1alpha1.ImageCompositionSpec{Layers: []ociv1alpha1.Layer{layer}})
			if err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestIntegrationDefaultsAreApplied — the defaults are part of the contract. If interval silently
// became zero the controller would requeue in a tight loop.
func TestIntegrationDefaultsAreApplied(t *testing.T) {
	obj := &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: "default"},
		Spec: ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:      "core",
				URLSource: &ociv1alpha1.URLSource{URL: "https://example.com/a.tgz"},
				Digest:    validDigest,
			}},
			Publish: &ociv1alpha1.Publish{Name: "defaults"},
		},
	}
	if err := k8s.Create(integrationCtx(t), obj); err != nil {
		t.Fatalf("creating: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(integrationCtx(t), obj) })

	if obj.Spec.Interval.Duration.Hours() != 1 {
		t.Errorf("interval defaulted to %v, want 1h", obj.Spec.Interval.Duration)
	}
	if obj.Spec.Layers[0].Unpack != ociv1alpha1.UnpackNone {
		t.Errorf("unpack defaulted to %q, want none", obj.Spec.Layers[0].Unpack)
	}
	if obj.Spec.Layers[0].Target != "/" {
		t.Errorf("target defaulted to %q, want /", obj.Spec.Layers[0].Target)
	}
	if obj.Spec.Publish.Tag != "latest" {
		t.Errorf("publish tag defaulted to %q, want latest", obj.Spec.Publish.Tag)
	}
}
