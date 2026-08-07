package controller

import (
	"context"
	"runtime"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// platformIndex builds a real multi-architecture index: children whose descriptors AND configs
// declare the given platforms. random.Index leaves descriptors empty, which is fine for asserting
// the refusal but useless for selecting a child.
func platformIndex(t *testing.T, platforms ...v1.Platform) v1.ImageIndex {
	t.Helper()
	idx := mutate.IndexMediaType(empty.Index, "application/vnd.oci.image.index.v1+json")
	for i, p := range platforms {
		img, err := random.Image(int64(64+i), 1)
		if err != nil {
			t.Fatalf("random image: %v", err)
		}
		cf, err := img.ConfigFile()
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		cf = cf.DeepCopy()
		cf.OS, cf.Architecture, cf.Variant = p.OS, p.Architecture, p.Variant
		img, err = mutate.ConfigFile(img, cf)
		if err != nil {
			t.Fatalf("mutate config: %v", err)
		}
		plat := p
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &plat},
		})
	}
	return idx
}

func publishIndex(t *testing.T, host, repo string, idx v1.ImageIndex) string {
	t.Helper()
	d, err := idx.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := remote.WriteIndex(mustRef(t, host+"/"+repo+"@"+d.String()), idx); err != nil {
		t.Fatalf("publishing index: %v", err)
	}
	return d.String()
}

// TestIndexBaseIsAcceptedWithPlatforms is the other half of TestMultiArchIndexIsRejected.
//
// ADR 0015 refuses an index base because resolving one would mean the CONTROLLER choosing a
// platform. With spec.platforms set the choice comes from the spec, so the refusal does not apply.
// The two tests together are what make that refusal conditional rather than absolute.
func TestIndexBaseIsAcceptedWithPlatforms(t *testing.T) {
	obj := composition("indexbase")
	r, host := servingReconciler(t, obj)

	idx := platformIndex(t,
		v1.Platform{OS: "linux", Architecture: "amd64"},
		v1.Platform{OS: "linux", Architecture: "arm64"},
	)
	digest := publishIndex(t, host, "multibase", idx)

	withBase(obj, host+"/multibase", digest)
	obj.Spec.Platforms = []string{"linux/amd64", "linux/arm64"}
	obj.Spec.Layers = []ociv1alpha1.Layer{removeLayer("noop", "/placeholder")}

	res, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("an index base with spec.platforms should be accepted: %v", err)
	}
	if res.Artifact == nil || res.Artifact.Digest == "" {
		t.Fatal("no artifact was published")
	}
	if res.Record == nil || len(res.Record.Manifests) != 2 {
		t.Fatalf("the build record must name one child per platform, got %+v", res.Record)
	}
}

// TestMultiPlatformPublishesAnIndex checks what a consumer sees, and — more importantly — that the
// children are recorded. An index whose children are unrecorded still resolves and still passes a
// HEAD; it fails at pull time, after garbage collection has swept them.
func TestMultiPlatformPublishesAnIndex(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("multiout", urlLayer("core", url, digest, "/core"))
	obj.Spec.Platforms = []string{"linux/amd64", "linux/arm64"}
	r, _ := servingReconciler(t, obj)

	res, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Record == nil {
		t.Fatal("no build record")
	}
	if len(res.Record.Manifests) != 2 {
		t.Fatalf("want 2 child manifests recorded, got %d: %v",
			len(res.Record.Manifests), res.Record.Manifests)
	}
	for _, child := range res.Record.Manifests {
		if child == res.Record.Digest {
			t.Fatal("a child digest equals the index digest")
		}
	}

	// Two children over ONE shared layer: two distinct configs plus one layer. Fewer than three
	// blobs would mean a child's config went unrecorded, and GC would reclaim it under a live
	// index.
	if len(res.Record.Blobs) != 3 {
		t.Fatalf("want 2 configs + 1 shared layer recorded, got %d: %v",
			len(res.Record.Blobs), res.Record.Blobs)
	}
}

// TestSinglePlatformStaysAnImage — naming exactly one platform must NOT wrap the result in an
// index. One platform is one image; wrapping would change the digest of every artifact that later
// adopts an explicit platform, for no gain.
func TestSinglePlatformStaysAnImage(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("singleplat", urlLayer("core", url, digest, "/core"))
	obj.Spec.Platforms = []string{"linux/amd64"}
	r, _ := servingReconciler(t, obj)

	res, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Record == nil {
		t.Fatal("no build record")
	}
	if len(res.Record.Manifests) != 0 {
		t.Fatalf("a single-platform build must record no child manifests, got %v",
			res.Record.Manifests)
	}
}

// TestExplicitAmd64MatchesTheDefault — on an amd64 controller, naming linux/amd64 explicitly must
// produce the same artifact as leaving platforms unset. If it did not, adopting the field would
// silently republish every artifact.
func TestExplicitAmd64MatchesTheDefault(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})

	unset := composition("plat-unset", urlLayer("core", url, digest, "/core"))
	r1, _ := servingReconciler(t, unset)
	res1, err := r1.reconcileArtifact(context.Background(), unset)
	if err != nil {
		t.Fatalf("unset: %v", err)
	}

	explicit := composition("plat-explicit", urlLayer("core", url, digest, "/core"))
	explicit.Spec.Platforms = []string{"linux/" + hostArch()}
	r2, _ := servingReconciler(t, explicit)
	res2, err := r2.reconcileArtifact(context.Background(), explicit)
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}

	if res1.Artifact.Digest != res2.Artifact.Digest {
		t.Fatalf("unset and explicit %s produced different artifacts:\n  %s\n  %s",
			hostArch(), res1.Artifact.Digest, res2.Artifact.Digest)
	}
}

// TestUnknownPlatformIsTerminal — a platform the base index does not offer needs a spec change, so
// retrying hourly would repeat the same failure. Substituting a near match is how an amd64 binary
// ends up on an arm node.
func TestUnknownPlatformIsTerminal(t *testing.T) {
	obj := composition("badplat")
	r, host := servingReconciler(t, obj)

	idx := platformIndex(t, v1.Platform{OS: "linux", Architecture: "amd64"})
	digest := publishIndex(t, host, "someindex", idx)

	withBase(obj, host+"/someindex", digest)
	obj.Spec.Platforms = []string{"linux/amd64", "linux/s390x"}
	obj.Spec.Layers = []ociv1alpha1.Layer{removeLayer("noop", "/placeholder")}

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil {
		t.Fatal("a platform the base does not offer was accepted")
	}
	if !strings.Contains(err.Error(), "s390x") {
		t.Fatalf("the error does not name the missing platform: %v", err)
	}
	var te *terminalError
	if !asTerminalErr(err, &te) {
		t.Fatalf("a missing platform needs a spec change, so it must be terminal: %v", err)
	}
}

// TestDuplicatePlatformIsTerminal — two identical children make the index ambiguous, and a puller
// choosing between them decides which of two identical things it meant.
func TestDuplicatePlatformIsTerminal(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("dupplat", urlLayer("core", url, digest, "/core"))
	obj.Spec.Platforms = []string{"linux/amd64", "linux/amd64"}
	r, _ := servingReconciler(t, obj)

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil {
		t.Fatal("a duplicated platform was accepted")
	}
	var te *terminalError
	if !asTerminalErr(err, &te) {
		t.Fatalf("a duplicated platform needs a spec change, so it must be terminal: %v", err)
	}
}

// TestBaseDigestChangeRebuilds covers the bug this work fixed: the base reached the output but not
// the input hash, so repointing spec.base.digest short-circuited as "unchanged" and the new base
// was never built.
func TestBaseDigestChangeRebuilds(t *testing.T) {
	obj := composition("baseswap")
	r, host := servingReconciler(t, obj)
	obj.Spec.Layers = []ociv1alpha1.Layer{removeLayer("noop", "/placeholder")}

	first, err := random.Image(128, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	d1, err := first.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := remote.Write(mustRef(t, host+"/swap@"+d1.String()), first); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	second, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	d2, err := second.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := remote.Write(mustRef(t, host+"/swap@"+d2.String()), second); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	withBase(obj, host+"/swap", d1.String())
	res1 := build(t, r, obj, "first base")

	withBase(obj, host+"/swap", d2.String())
	res2, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("second base: %v", err)
	}

	if res1.Digest == res2.Artifact.Digest {
		t.Fatal("repointing the base produced the same artifact; the base is not in the input hash")
	}
}

// hostArch is the architecture the test process runs on, which is also what an unset platform
// list resolves to.
func hostArch() string { return runtime.GOARCH }
