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

const validDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func fetchLayer(name string) ociv1alpha1.Layer {
	return ociv1alpha1.Layer{
		Name:  name,
		Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: validDigest},
		To:    "/x",
	}
}

// TestIntegrationValidSpecIsAccepted anchors the negative cases below: without it, a rule that
// rejected everything would look like a complete pass.
func TestIntegrationValidSpecIsAccepted(t *testing.T) {
	if err := apply(t, "valid", ociv1alpha1.ImageCompositionSpec{
		Layers:  []ociv1alpha1.Layer{fetchLayer("core")},
		Publish: &ociv1alpha1.Publish{Name: "valid", Tag: "main"},
	}); err != nil {
		t.Fatalf("a valid spec was rejected: %v", err)
	}
}

// TestIntegrationVerbUnionIsEnforced — exactly one of fetch, configMap, sourceRef, remove. This
// keeps the discriminated union honest, and nothing else checks it.
func TestIntegrationVerbUnionIsEnforced(t *testing.T) {
	t.Run("no verb", func(t *testing.T) {
		err := apply(t, "no-verb", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{Name: "empty", To: "/x"}},
		})
		if err == nil {
			t.Fatal("a layer with no verb was accepted")
		}
		if !strings.Contains(err.Error(), "exactly one of") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("two verbs", func(t *testing.T) {
		if err := apply(t, "two-verbs", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:      "both",
				Fetch:     &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: validDigest},
				ConfigMap: &ociv1alpha1.ConfigMapSource{Name: "cm"},
				To:        "/x",
			}},
		}); err == nil {
			t.Fatal("a layer with two verbs was accepted")
		}
	})
}

// TestIntegrationPlacementRules — `to` is required for content and forbidden for remove, and
// owner/mode do not apply to remove. One CEL rule covers all of it; this checks each arm.
func TestIntegrationPlacementRules(t *testing.T) {
	t.Run("content without to is rejected", func(t *testing.T) {
		if err := apply(t, "no-to", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:  "core",
				Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: validDigest},
			}},
		}); err == nil {
			t.Fatal("a content layer without 'to' was accepted")
		}
	})

	t.Run("remove with to is rejected", func(t *testing.T) {
		if err := apply(t, "remove-to", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{Name: "prune", Remove: []string{"/a"}, To: "/x"}},
		}); err == nil {
			t.Fatal("a remove layer with 'to' was accepted")
		}
	})

	t.Run("remove with owner is rejected", func(t *testing.T) {
		if err := apply(t, "remove-owner", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name: "prune", Remove: []string{"/a"},
				Owner: &ociv1alpha1.Ownership{UID: 1001},
			}},
		}); err == nil {
			t.Fatal("a remove layer with 'owner' was accepted")
		}
	})

	t.Run("remove with mode is rejected", func(t *testing.T) {
		if err := apply(t, "remove-mode", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name: "prune", Remove: []string{"/a"},
				Mode: &ociv1alpha1.FileMode{File: "0644"},
			}},
		}); err == nil {
			t.Fatal("a remove layer with 'mode' was accepted")
		}
	})

	t.Run("a plain remove is accepted", func(t *testing.T) {
		if err := apply(t, "remove-ok", ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{Name: "prune", Remove: []string{"/opt/old.jar"}}},
		}); err != nil {
			t.Fatalf("a valid remove layer was rejected: %v", err)
		}
	})
}

// TestIntegrationResolvedSourcesNeedNoDigest — sourceRef and configMap are content-addressed by
// the cluster, so their digests are resolved rather than declared. See ADR 0002.
func TestIntegrationResolvedSourcesNeedNoDigest(t *testing.T) {
	if err := apply(t, "resolved", ociv1alpha1.ImageCompositionSpec{
		Layers: []ociv1alpha1.Layer{
			{Name: "settings", ConfigMap: &ociv1alpha1.ConfigMapSource{Name: "cm"}, To: "/config"},
			{
				Name: "overlay", To: "/etc",
				SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "repo"},
			},
		},
	}); err != nil {
		t.Fatalf("resolved-digest sources were rejected: %v", err)
	}
}

// TestIntegrationBaseNeedsADigest — the base is content-addressed like every other input.
func TestIntegrationBaseNeedsADigest(t *testing.T) {
	if err := apply(t, "base-no-digest", ociv1alpha1.ImageCompositionSpec{
		Base:   &ociv1alpha1.BaseImage{Image: "quay.io/strimzi/kafka"},
		Layers: []ociv1alpha1.Layer{fetchLayer("core")},
	}); err == nil {
		t.Fatal("a base without a digest was accepted")
	}
}

// TestIntegrationInheritNeedsABase — there is nothing to inherit from otherwise, and a silently
// empty config would leave a non-runnable image with no explanation.
func TestIntegrationInheritNeedsABase(t *testing.T) {
	err := apply(t, "inherit-no-base", ociv1alpha1.ImageCompositionSpec{
		Layers: []ociv1alpha1.Layer{fetchLayer("core")},
		Config: &ociv1alpha1.ImageConfig{Inherit: true},
	})
	if err == nil {
		t.Fatal("config.inherit without a base was accepted")
	}
	if !strings.Contains(err.Error(), "requires a base") {
		t.Fatalf("the error does not explain why: %v", err)
	}
}

// TestIntegrationBaseWithInheritIsAccepted — the runnable-image shape.
func TestIntegrationBaseWithInheritIsAccepted(t *testing.T) {
	if err := apply(t, "runnable", ociv1alpha1.ImageCompositionSpec{
		Base: &ociv1alpha1.BaseImage{
			Image:     "quay.io/strimzi/kafka",
			Digest:    validDigest,
			SecretRef: &ociv1alpha1.LocalObjectReference{Name: "kafka-pull"},
		},
		Layers: []ociv1alpha1.Layer{fetchLayer("plugins")},
		Config: &ociv1alpha1.ImageConfig{
			Inherit:      true,
			User:         "1001",
			WorkingDir:   "/opt/kafka",
			ExposedPorts: []string{"9092/tcp"},
			StopSignal:   "SIGTERM",
		},
		Publish: &ociv1alpha1.Publish{Name: "runnable", Tag: "main"},
	}); err != nil {
		t.Fatalf("the runnable-image shape was rejected: %v", err)
	}
}

// TestIntegrationPushAndPublishAreMutuallyExclusive — setting both leaves it ambiguous where the
// artifact goes and what status.artifact.ref should say.
func TestIntegrationPushAndPublishAreMutuallyExclusive(t *testing.T) {
	if err := apply(t, "both-targets", ociv1alpha1.ImageCompositionSpec{
		Layers:  []ociv1alpha1.Layer{fetchLayer("core")},
		Publish: &ociv1alpha1.Publish{Name: "x", Tag: "main"},
		Push:    &ociv1alpha1.Push{Repository: "ghcr.io/example/x", Tag: "main"},
	}); err == nil {
		t.Fatal("both push and publish were accepted")
	}
}

// TestIntegrationEmptyLayersIsRejected — an empty list is far more likely to be a templating
// accident than a deliberate empty artifact.
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
			Name: "x", To: "/x",
			Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: "md5:abcd"},
		},
		"short digest": {
			Name: "x", To: "/x",
			Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: "sha256:abcd"},
		},
		"non-http url": {
			Name: "x", To: "/x",
			Fetch: &ociv1alpha1.FetchSource{URL: "ftp://example.com/a.tgz", Digest: validDigest},
		},
		"relative to": {
			Name: "x", To: "relative/path",
			Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: validDigest},
		},
		"unknown unpack mode": {
			Name: "x", To: "/x",
			Fetch: &ociv1alpha1.FetchSource{
				URL: "https://example.com/a.tgz", Digest: validDigest, Unpack: "zip",
			},
		},
		"unknown source kind": {
			Name: "x", To: "/x",
			SourceRef: &ociv1alpha1.SourceRefSource{Kind: "HelmRepository", Name: "r"},
		},
		"non-octal file mode": {
			Name: "x", To: "/x",
			Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: validDigest},
			Mode:  &ociv1alpha1.FileMode{File: "rw-r--r--"},
		},
		"empty remove list": {
			Name: "x", Remove: []string{},
		},
	}

	for name, layer := range cases {
		t.Run(name, func(t *testing.T) {
			if err := apply(t, "malformed-"+strings.ReplaceAll(name, " ", "-"),
				ociv1alpha1.ImageCompositionSpec{Layers: []ociv1alpha1.Layer{layer}}); err == nil {
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
				Name:  "core",
				Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: validDigest},
				To:    "/x",
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
	if obj.Spec.Layers[0].Fetch.Unpack != ociv1alpha1.UnpackNone {
		t.Errorf("unpack defaulted to %q, want none", obj.Spec.Layers[0].Fetch.Unpack)
	}
	if obj.Spec.Publish.Tag != "latest" {
		t.Errorf("publish tag defaulted to %q, want latest", obj.Spec.Publish.Tag)
	}
}
