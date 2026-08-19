//go:build integration

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
		Publish: &ociv1alpha1.Publish{Name: "valid", Tags: []string{"main"}},
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

// TestIntegrationBaseSpellings — `ref` and `image`+`digest` are the two ways to name a base, and
// the CEL rules exist to stop a spec being ambiguous or half-written. The pair must go together,
// and naming the base twice must be refused rather than silently preferring one.
func TestIntegrationBaseSpellings(t *testing.T) {
	const pinned = "quay.io/strimzi/kafka:0.43.0@" + validDigest

	accepted := map[string]*ociv1alpha1.BaseImage{
		"ref alone":        {Ref: pinned},
		"image and digest": {Image: "quay.io/strimzi/kafka", Digest: validDigest},
	}
	for name, base := range accepted {
		t.Run(name, func(t *testing.T) {
			if err := apply(t, "base-ok-"+strings.ReplaceAll(name, " ", "-"),
				ociv1alpha1.ImageCompositionSpec{
					Base:   base,
					Layers: []ociv1alpha1.Layer{fetchLayer("x")},
				}); err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}

	refused := map[string]*ociv1alpha1.BaseImage{
		"both spellings":  {Ref: pinned, Image: "quay.io/strimzi/kafka", Digest: validDigest},
		"digest with ref": {Ref: pinned, Digest: validDigest},
		"image alone":     {Image: "quay.io/strimzi/kafka"},
		"digest alone":    {Digest: validDigest},
		"neither":         {},
		"unpinned ref":    {Ref: "quay.io/strimzi/kafka:0.43.0"},
	}
	for name, base := range refused {
		t.Run(name, func(t *testing.T) {
			if err := apply(t, "base-bad-"+strings.ReplaceAll(name, " ", "-"),
				ociv1alpha1.ImageCompositionSpec{
					Base:   base,
					Layers: []ociv1alpha1.Layer{fetchLayer("x")},
				}); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// TestIntegrationImageLayer — the image verb joins the layer union, so it must be accepted alone
// and refused alongside another verb. An unpinned ref must be refused too: an image layer is
// content-addressed like everything else (ADR 0002).
func TestIntegrationImageLayer(t *testing.T) {
	const pinned = "ghcr.io/lhns/app:v1@" + validDigest

	if err := apply(t, "image-layer-ok", ociv1alpha1.ImageCompositionSpec{
		Layers: []ociv1alpha1.Layer{{
			Name: "app", To: "/opt", Image: &ociv1alpha1.ImageSource{Ref: pinned},
		}},
	}); err != nil {
		t.Errorf("a valid image layer was rejected: %v", err)
	}

	refused := map[string]ociv1alpha1.Layer{
		"unpinned": {Name: "x", To: "/x", Image: &ociv1alpha1.ImageSource{Ref: "ghcr.io/lhns/app:v1"}},
		"with fetch": {
			Name: "x", To: "/x",
			Image: &ociv1alpha1.ImageSource{Ref: pinned},
			Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: validDigest},
		},
		"with remove": {
			Name: "x", Remove: []string{"/x"},
			Image: &ociv1alpha1.ImageSource{Ref: pinned},
		},
		"no target": {Name: "x", Image: &ociv1alpha1.ImageSource{Ref: pinned}},
	}
	for name, layer := range refused {
		t.Run(name, func(t *testing.T) {
			if err := apply(t, "image-layer-bad-"+strings.ReplaceAll(name, " ", "-"),
				ociv1alpha1.ImageCompositionSpec{Layers: []ociv1alpha1.Layer{layer}}); err == nil {
				t.Error("accepted")
			}
		})
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
		Publish: &ociv1alpha1.Publish{Name: "runnable", Tags: []string{"main"}},
	}); err != nil {
		t.Fatalf("the runnable-image shape was rejected: %v", err)
	}
}

// TestIntegrationPushAndPublishAreMutuallyExclusive — setting both leaves it ambiguous where the
// artifact goes and what status.artifact.ref should say.
func TestIntegrationPushAndPublishAreMutuallyExclusive(t *testing.T) {
	if err := apply(t, "both-targets", ociv1alpha1.ImageCompositionSpec{
		Layers:  []ociv1alpha1.Layer{fetchLayer("core")},
		Publish: &ociv1alpha1.Publish{Name: "x", Tags: []string{"main"}},
		Push:    &ociv1alpha1.Push{Repository: "ghcr.io/example/x", Tags: []string{"main"}},
	}); err == nil {
		t.Fatal("both push and publish were accepted")
	}
}

// TestIntegrationEmptyLayersIsRejected — an empty list is far more likely to be a templating
// accident than a deliberate empty artifact.
func TestIntegrationEmptyLayersIsRejected(t *testing.T) {
	if err := apply(t, "no-layers", ociv1alpha1.ImageCompositionSpec{
		Publish: &ociv1alpha1.Publish{Name: "x", Tags: []string{"main"}},
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
		// "rpm" rather than something arbitrary: it looks exactly like a mode that ought to work,
		// which is what makes it a good canary, and it stays invalid for as long as RPM support is
		// declined (issue #9, ADR 0022). This slot previously held "zip", which stopped being a
		// useful canary the moment zip was implemented.
		"unknown unpack mode": {
			Name: "x", To: "/x",
			Fetch: &ociv1alpha1.FetchSource{
				URL: "https://example.com/a.tgz", Digest: validDigest, Unpack: "rpm",
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

// TestIntegrationEveryUnpackModeIsAccepted — the CRD's enum and the controller's switch are
// separate hand-maintained lists, and this is the half only a real API server can check. Forget the
// kubebuilder marker and the mode is unusable no matter how complete the implementation is; the
// symptom is a rejection at apply time that every unit test passes straight through.
//
// Paired with TestUnknownUnpackModeIsTerminal in internal/oci, which covers the other direction.
func TestIntegrationEveryUnpackModeIsAccepted(t *testing.T) {
	// allUnpackModes, not a local copy: unpackparity_test.go carries no build tag, so it compiles
	// into this build too. A second list here would be one that silently falls behind, quietly
	// dropping coverage of whichever mode was added to only one of them.
	for _, mode := range allUnpackModes {
		t.Run(string(mode), func(t *testing.T) {
			err := apply(t, "unpack-"+strings.ReplaceAll(string(mode), ".", "-"),
				ociv1alpha1.ImageCompositionSpec{
					Layers: []ociv1alpha1.Layer{{
						Name: "x", To: "/x/file",
						Fetch: &ociv1alpha1.FetchSource{
							URL: "https://example.com/a", Digest: validDigest, Unpack: mode,
						},
					}},
				})
			if err != nil {
				t.Errorf("unpack %q was rejected: %v", mode, err)
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

	if obj.Spec.Interval == nil || obj.Spec.Interval.Duration.Hours() != 1 {
		t.Errorf("interval defaulted to %v, want 1h", obj.Spec.Interval.Duration)
	}
	if obj.Spec.Layers[0].Fetch.Unpack != ociv1alpha1.UnpackNone {
		t.Errorf("unpack defaulted to %q, want none", obj.Spec.Layers[0].Fetch.Unpack)
	}
	// No tags by default: publishing by digest alone is the safe floor, and inventing a "latest"
	// nobody asked for would make an unreferenced mutable tag appear on every object.
	if len(obj.Spec.Publish.Tags) != 0 {
		t.Errorf("publish tags defaulted to %v, want none", obj.Spec.Publish.Tags)
	}
	// ...but the conflict policy resolves to Fail, so a tag cannot be silently remeaned by
	// accident. Asserted through the resolver rather than through a materialised field value,
	// because onConflict deliberately has no schema default -- see
	// TestIntegrationOnConflictHasNoSchemaDefault for why.
	if got := obj.Spec.Publish.ResolveConflictPolicy(); got != ociv1alpha1.ConflictFail {
		t.Errorf("an object that says nothing resolves to %q, want Fail", got)
	}
}

// TestIntegrationImmutableFalseSurvivesTheRoundTrip — immutable is a *bool precisely so that an
// explicit false is not swallowed. A plain bool with omitempty would serialise false as absent and
// a deliberately moving tag would start failing its own builds. Exactly the bug this project
// already hit once with interval.
//
// Still worth running now that the field is deprecated, and arguably more so: this is the shape of
// every object written before onConflict existed, and the whole claim of the deprecation is that
// they keep working untouched.
func TestIntegrationImmutableFalseSurvivesTheRoundTrip(t *testing.T) {
	obj := &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{Name: "moving-pointer", Namespace: "default"},
		Spec: ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:  "core",
				Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: validDigest},
				To:    "/x",
			}},
			Publish: &ociv1alpha1.Publish{
				Name:      "moving-pointer",
				Tags:      []string{"main"},
				Immutable: ptr.To(false),
			},
		},
	}
	if err := k8s.Create(integrationCtx(t), obj); err != nil {
		t.Fatalf("creating: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(integrationCtx(t), obj) })

	fetched := &ociv1alpha1.ImageComposition{}
	if err := k8s.Get(integrationCtx(t), client.ObjectKeyFromObject(obj), fetched); err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if fetched.Spec.Publish.Immutable == nil {
		t.Fatal("explicit immutable: false was dropped on the wire and defaulted back to true")
	}
	if got := fetched.Spec.Publish.ResolveConflictPolicy(); got != ociv1alpha1.ConflictOverwrite {
		t.Errorf("a deprecated immutable:false resolved to %q, want Overwrite; objects written "+
			"before onConflict existed must keep behaving as they did", got)
	}
}
