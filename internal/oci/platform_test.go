package oci

import (
	"runtime"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// testInputs is one platform-neutral bundle — the shape every artifact in the estate actually has.
func testInputs(t *testing.T) ([]LayerInput, string) {
	t.Helper()
	return []LayerInput{{
		Name:   "bundle",
		Digest: "sha256:1111",
		Unpack: UnpackTarGz,
		Target: "/plugins",
		Path:   writeTarGz(t, map[string]string{"core.jar": "jar bytes"}),
	}}, t.TempDir()
}

// TestUnsetPlatformMatchesTheOldHardcodedDefault is the regression guard for the rollout.
//
// Before multi-architecture output, a base-less artifact was stamped linux/amd64 unconditionally.
// It is now stamped with the CONTROLLER's platform. Those must agree on amd64, or upgrading the
// controller republishes different content under spec-hash tags that have not changed — which
// `immutable: true` turns into a failed build for every artifact in every cluster at once.
//
// Skipped off amd64 rather than deleted: the property being asserted is specifically "the new
// default equals the old constant on the architecture everything was built on".
//
// GOOS is deliberately NOT part of the condition. The OS is always linux — see RuntimePlatform,
// which does not use runtime.GOOS precisely so that building the controller on Windows or macOS
// cannot stamp an artifact nothing will run. This test therefore holds on any amd64 host.
func TestUnsetPlatformMatchesTheOldHardcodedDefault(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skipf("the old default was linux/amd64; this host is %s", runtime.GOARCH)
	}

	inputs, dir := testInputs(t)
	img, err := Assemble(nil, inputs, Config{}, dir)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cf.OS != "linux" || cf.Architecture != "amd64" {
		t.Fatalf("a base-less artifact must still be linux/amd64 here, got %s/%s",
			cf.OS, cf.Architecture)
	}
	if cf.Variant != "" {
		t.Fatalf("variant should be empty, got %q", cf.Variant)
	}
}

// TestPlatformComesFromTheBase keeps ADR 0015's rule: claiming amd64 over an arm64 base produces
// an image the kubelet refuses to run, and the error points at the workload rather than here.
func TestPlatformComesFromTheBase(t *testing.T) {
	base, err := random.Image(64, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	cf, err := base.ConfigFile()
	if err != nil {
		t.Fatalf("base config: %v", err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture, cf.Variant = "linux", "arm64", "v8"
	base, err = mutate.ConfigFile(base, cf)
	if err != nil {
		t.Fatalf("mutate base: %v", err)
	}

	inputs, dir := testInputs(t)
	img, err := Assemble(base, inputs, Config{}, dir)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	got, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if got.OS != "linux" || got.Architecture != "arm64" || got.Variant != "v8" {
		t.Fatalf("want linux/arm64/v8 from the base, got %s/%s/%s",
			got.OS, got.Architecture, got.Variant)
	}
}

// TestAssembleIndexIsDeterministic is the load-bearing test of the project, applied to the index
// path: two assemblies of identical inputs must produce the same index digest, or nothing above it
// — skipping rebuilds, comparing digests, immutable tags — holds.
func TestAssembleIndexIsDeterministic(t *testing.T) {
	platforms := []Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}

	digestOf := func() string {
		inputs, dir := testInputs(t)
		idx, err := AssembleIndex(nil, inputs, Config{}, platforms, dir)
		if err != nil {
			t.Fatalf("assemble index: %v", err)
		}
		d, err := idx.Digest()
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		return d.String()
	}

	if a, b := digestOf(), digestOf(); a != b {
		t.Fatalf("index digest is not deterministic:\n  %s\n  %s", a, b)
	}
}

// TestAssembleIndexStampsEachPlatform checks the descriptors a kubelet actually reads. Getting
// these wrong produces "no matching manifest for linux/arm64", which points at the workload.
func TestAssembleIndexStampsEachPlatform(t *testing.T) {
	platforms := []Platform{
		{OS: "linux", Architecture: "amd64"},
		{OS: "linux", Architecture: "arm", Variant: "v7"},
	}
	inputs, dir := testInputs(t)
	idx, err := AssembleIndex(nil, inputs, Config{}, platforms, dir)
	if err != nil {
		t.Fatalf("assemble index: %v", err)
	}

	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatalf("index manifest: %v", err)
	}
	if len(im.Manifests) != 2 {
		t.Fatalf("want 2 children, got %d", len(im.Manifests))
	}

	for i, want := range platforms {
		desc := im.Manifests[i]
		if desc.Platform == nil {
			t.Fatalf("child %d has no platform descriptor", i)
		}
		got := Platform{OS: desc.Platform.OS, Architecture: desc.Platform.Architecture, Variant: desc.Platform.Variant}
		if got != want {
			t.Fatalf("child %d descriptor is %s, want %s", i, got, want)
		}
		// And the child's own config must agree with the descriptor. A descriptor that lies is
		// worse than no index: the puller selects on it and then runs the wrong binary.
		child, err := idx.Image(desc.Digest)
		if err != nil {
			t.Fatalf("child %d: %v", i, err)
		}
		cf, err := child.ConfigFile()
		if err != nil {
			t.Fatalf("child %d config: %v", i, err)
		}
		inConfig := Platform{OS: cf.OS, Architecture: cf.Architecture, Variant: cf.Variant}
		if inConfig != want {
			t.Fatalf("child %d config says %s but the descriptor says %s", i, inConfig, want)
		}
	}
}

// TestAssembleIndexSharesLayers asserts that composed content is byte-identical across platforms.
// The layers are the same files by construction; if they ever stopped being shared, an index would
// silently double its storage and the children would disagree about content they represent
// equally.
func TestAssembleIndexSharesLayers(t *testing.T) {
	platforms := []Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}
	inputs, dir := testInputs(t)
	idx, err := AssembleIndex(nil, inputs, Config{}, platforms, dir)
	if err != nil {
		t.Fatalf("assemble index: %v", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatalf("index manifest: %v", err)
	}

	var first []v1.Hash
	for i, desc := range im.Manifests {
		child, err := idx.Image(desc.Digest)
		if err != nil {
			t.Fatalf("child %d: %v", i, err)
		}
		layers, err := child.Layers()
		if err != nil {
			t.Fatalf("child %d layers: %v", i, err)
		}
		digests := make([]v1.Hash, 0, len(layers))
		for _, l := range layers {
			d, err := l.Digest()
			if err != nil {
				t.Fatalf("layer digest: %v", err)
			}
			digests = append(digests, d)
		}
		if i == 0 {
			first = digests
			continue
		}
		if len(digests) != len(first) {
			t.Fatalf("child %d has %d layers, child 0 has %d", i, len(digests), len(first))
		}
		for j := range digests {
			if digests[j] != first[j] {
				t.Fatalf("child %d layer %d is %s, child 0 has %s", i, j, digests[j], first[j])
			}
		}
	}
}

// TestParsePlatform covers the spec's two accepted shapes and rejects the rest. The CRD pattern
// enforces this too; this is the belt to that braces, since a spec that somehow passed validation
// still must not produce a half-parsed platform.
func TestParsePlatform(t *testing.T) {
	ok := map[string]Platform{
		"linux/amd64":  {OS: "linux", Architecture: "amd64"},
		"linux/arm/v7": {OS: "linux", Architecture: "arm", Variant: "v7"},
	}
	for in, want := range ok {
		got, err := ParsePlatform(in)
		if err != nil {
			t.Fatalf("ParsePlatform(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParsePlatform(%q) = %+v, want %+v", in, got, want)
		}
		if got.String() != in {
			t.Fatalf("round trip of %q gave %q", in, got.String())
		}
	}
	for _, bad := range []string{"", "linux", "linux/arm/v7/extra"} {
		if _, err := ParsePlatform(bad); err == nil {
			t.Fatalf("ParsePlatform(%q) should have failed", bad)
		}
	}
}

// TestPlatformsAreInTheInputHash: a build for a different platform set produces different output,
// so it must produce a different hash. Otherwise the second build is skipped as "unchanged" and
// the wrong artifact is served indefinitely.
func TestPlatformsAreInTheInputHash(t *testing.T) {
	layers := []LayerInput{{Digest: "sha256:1111", Unpack: UnpackNone, Target: "/x"}}

	amd := InputHash(layers, Config{}, "", []Platform{{OS: "linux", Architecture: "amd64"}})
	arm := InputHash(layers, Config{}, "", []Platform{{OS: "linux", Architecture: "arm64"}})
	both := InputHash(layers, Config{}, "", []Platform{
		{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"},
	})

	if amd == arm {
		t.Fatal("amd64 and arm64 share an input hash")
	}
	if amd == both || arm == both {
		t.Fatal("a two-platform index shares an input hash with a single-platform image")
	}

	// Order is part of the identity: it decides the order of the children in the index, and so
	// the index digest.
	rev := InputHash(layers, Config{}, "", []Platform{
		{OS: "linux", Architecture: "arm64"}, {OS: "linux", Architecture: "amd64"},
	})
	if rev == both {
		t.Fatal("platform order does not affect the input hash, but it affects the index digest")
	}
}

// TestBaseDigestIsInTheInputHash covers a bug this change fixed: the base reached the output but
// not the hash, so repointing spec.base.digest left the hash unchanged, the cheap path
// short-circuited, and the new base was silently never built.
func TestBaseDigestIsInTheInputHash(t *testing.T) {
	layers := []LayerInput{{Digest: "sha256:1111", Unpack: UnpackNone, Target: "/x"}}
	a := InputHash(layers, Config{}, "sha256:aaaa", nil)
	b := InputHash(layers, Config{}, "sha256:bbbb", nil)
	if a == b {
		t.Fatal("changing the base digest must change the input hash")
	}
	if none := InputHash(layers, Config{}, "", nil); none == a {
		t.Fatal("having a base must differ from having none")
	}
}
