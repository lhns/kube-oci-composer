package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// publishBaseImage puts a real image into the test registry and returns its repository and digest,
// standing in for something like a Strimzi Kafka image.
func publishBaseImage(t *testing.T, host, repo string, layers int, cfg *v1.Config) (string, string) {
	t.Helper()

	img, err := random.Image(512, int64(layers))
	if err != nil {
		t.Fatalf("building base image: %v", err)
	}
	if cfg != nil {
		cf, err := img.ConfigFile()
		if err != nil {
			t.Fatalf("reading config: %v", err)
		}
		cf = cf.DeepCopy()
		cf.Config = *cfg
		cf.OS, cf.Architecture = "linux", "arm64"
		if img, err = mutate.ConfigFile(img, cf); err != nil {
			t.Fatalf("setting config: %v", err)
		}
	}

	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	ref, err := name.ParseReference(host+"/"+repo+"@"+digest.String(), name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("publishing base image: %v", err)
	}
	return host + "/" + repo, digest.String()
}

// withBase sets the hoisted base image. It is no longer a layer entry — see ADR 0016.
func withBase(obj *ociv1alpha1.ImageComposition, repository, digest string) {
	obj.Spec.Base = &ociv1alpha1.BaseImage{Image: repository, Digest: digest}
}

// removeLayer deletes paths inherited from the base.
func removeLayer(name string, paths ...string) ociv1alpha1.Layer {
	return ociv1alpha1.Layer{Name: name, Remove: paths}
}

// TestBaseImageLayersAreContributed is the Kafka-plus-plugins case: an upstream image, with our
// content added on top, producing something runnable rather than a bundle to mount.
func TestBaseImageLayersAreContributed(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"plugins/core.jar": "jar"})

	obj := composition("layered")
	r, host := servingReconciler(t, obj)

	baseRepo, baseDigest := publishBaseImage(t, host, "kafka", 3, nil)
	withBase(obj, baseRepo, baseDigest)
	obj.Spec.Layers = []ociv1alpha1.Layer{
		urlLayer("plugins", origin.url, origin.digest, "/plugins"),
	}

	art := build(t, r, obj, "compose over a base image")

	ref, err := name.ParseReference(host+"/layered@"+art.Digest, name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	img, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("pulling the result: %v", err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("reading layers: %v", err)
	}

	// Three from the base, one of ours.
	if len(layers) != 4 {
		t.Fatalf("composed image has %d layers, want 4 (3 base + 1 added)", len(layers))
	}

	// The base's layers must be reused verbatim. Repacking them would change their digests,
	// break sharing with anything else on the same base, and force re-uploading content the
	// registry already holds.
	base, err := remote.Image(mustRef(t, baseRepo+"@"+baseDigest))
	if err != nil {
		t.Fatalf("pulling the base: %v", err)
	}
	baseLayers, err := base.Layers()
	if err != nil {
		t.Fatalf("reading base layers: %v", err)
	}
	for i, bl := range baseLayers {
		want, _ := bl.Digest()
		got, _ := layers[i].Digest()
		if got != want {
			t.Fatalf("layer %d is %s, want the base's %s — base layers were repacked", i, got, want)
		}
	}
}

// TestBaseAlwaysComesFirst — hoisting the base out of the list means its layers are always
// underneath, which is what "base" means. Nothing in the spec can reorder that.
func TestBaseAlwaysComesFirst(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"a.txt": "a"})

	obj := composition("order")
	r, host := servingReconciler(t, obj)
	baseRepo, baseDigest := publishBaseImage(t, host, "base", 2, nil)

	withBase(obj, baseRepo, baseDigest)
	obj.Spec.Layers = []ociv1alpha1.Layer{urlLayer("files", origin.url, origin.digest, "/files")}
	art := build(t, r, obj, "build")

	img, err := remote.Image(mustRef(t, host+"/order@"+art.Digest))
	if err != nil {
		t.Fatalf("pulling: %v", err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("layers: %v", err)
	}
	if len(layers) != 3 {
		t.Fatalf("got %d layers, want 3 (2 base + 1 added)", len(layers))
	}

	base, err := remote.Image(mustRef(t, baseRepo+"@"+baseDigest))
	if err != nil {
		t.Fatalf("pulling base: %v", err)
	}
	baseLayers, _ := base.Layers()
	for i, bl := range baseLayers {
		want, _ := bl.Digest()
		got, _ := layers[i].Digest()
		if got != want {
			t.Fatalf("layer %d is not the base's: %s vs %s", i, got, want)
		}
	}
}

// TestConfigFromInheritsTheBase is what makes the result runnable. Without the base's entrypoint,
// env and user, a composed image starts and immediately fails.
func TestConfigFromInheritsTheBase(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"plugins/core.jar": "jar"})

	obj := composition("runnable")
	r, host := servingReconciler(t, obj)

	baseRepo, baseDigest := publishBaseImage(t, host, "kafka", 2, &v1.Config{
		Entrypoint: []string{"/opt/kafka/bin/kafka-server-start.sh"},
		Env:        []string{"KAFKA_HOME=/opt/kafka"},
		User:       "1001",
		WorkingDir: "/opt/kafka",
	})

	withBase(obj, baseRepo, baseDigest)
	obj.Spec.Layers = []ociv1alpha1.Layer{
		urlLayer("plugins", origin.url, origin.digest, "/plugins"),
	}
	obj.Spec.Config = &ociv1alpha1.ImageConfig{Inherit: true}

	art := build(t, r, obj, "compose with inherited config")

	img, err := remote.Image(mustRef(t, host+"/runnable@"+art.Digest))
	if err != nil {
		t.Fatalf("pulling: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}

	if len(cf.Config.Entrypoint) == 0 || cf.Config.Entrypoint[0] != "/opt/kafka/bin/kafka-server-start.sh" {
		t.Fatalf("entrypoint was not inherited: %v", cf.Config.Entrypoint)
	}
	if cf.Config.User != "1001" {
		t.Fatalf("user %q was not inherited", cf.Config.User)
	}
	if cf.Config.WorkingDir != "/opt/kafka" {
		t.Fatalf("working dir %q was not inherited", cf.Config.WorkingDir)
	}
	// The platform comes from the base too. Claiming amd64 over an arm64 base produces an image
	// the kubelet refuses to run, for a reason that points nowhere useful.
	if cf.Architecture != "arm64" {
		t.Fatalf("architecture is %q, want the base's arm64", cf.Architecture)
	}
}

// TestWithoutConfigFromTheConfigStaysEmpty — inheritance is opt-in. For a bundle that is only
// mounted, silently adopting a base's entrypoint would be surprising.
func TestWithoutConfigFromTheConfigStaysEmpty(t *testing.T) {
	obj := composition("bundle")
	r, host := servingReconciler(t, obj)

	baseRepo, baseDigest := publishBaseImage(t, host, "kafka", 1, &v1.Config{
		Entrypoint: []string{"/entrypoint.sh"},
	})
	withBase(obj, baseRepo, baseDigest)
	obj.Spec.Layers = []ociv1alpha1.Layer{removeLayer("noop", "/nonexistent-placeholder")}

	art := build(t, r, obj, "compose without inherit")

	img, err := remote.Image(mustRef(t, host+"/bundle@"+art.Digest))
	if err != nil {
		t.Fatalf("pulling: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if len(cf.Config.Entrypoint) != 0 {
		t.Fatalf("entrypoint %v was inherited without config.from", cf.Config.Entrypoint)
	}
}

// TestInheritWithoutABaseIsRejected — there is nothing to inherit from, and silently producing
// an empty config would leave a non-runnable image with no explanation.
func TestInheritWithoutABaseIsRejected(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"a": "1"})
	obj := composition("noinherit", urlLayer("files", origin.url, origin.digest, "/files"))
	obj.Spec.Config = &ociv1alpha1.ImageConfig{Inherit: true}
	r, _ := servingReconciler(t, obj)

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil || !strings.Contains(err.Error(), "no base to inherit from") {
		t.Fatalf("expected a clear error, got %v", err)
	}
}

// TestBaseImageIsNotPulledWhenNothingChanged — the short-circuit must cover image entries too, or
// every interval would mean a registry round trip per base image.
func TestBaseImageIsNotPulledWhenNothingChanged(t *testing.T) {
	obj := composition("steady-base")
	r, host := servingReconciler(t, obj)
	baseRepo, baseDigest := publishBaseImage(t, host, "kafka", 1, nil)
	withBase(obj, baseRepo, baseDigest)
	obj.Spec.Layers = []ociv1alpha1.Layer{removeLayer("noop", "/nonexistent-placeholder")}

	first := build(t, r, obj, "first build")

	// Delete the base from the registry. A reconcile that still pulls it will fail.
	if err := remote.Delete(mustRef(t, baseRepo+"@"+baseDigest)); err != nil {
		t.Skipf("test registry does not support delete: %v", err)
	}

	second := build(t, r, obj, "steady-state reconcile")
	if second.Digest != first.Digest {
		t.Fatalf("digest changed: %s then %s", first.Digest, second.Digest)
	}
}

// TestMultiArchIndexIsRejected — go-containerregistry would happily pick a platform for us, which
// is exactly the problem: the choice would come from the controller's defaults rather than the
// spec, so the same object could produce different output on different builds.
func TestMultiArchIndexIsRejected(t *testing.T) {
	obj := composition("multiarch")
	r, host := servingReconciler(t, obj)

	idx, err := random.Index(512, 1, 2)
	if err != nil {
		t.Fatalf("building index: %v", err)
	}
	digest, err := idx.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := remote.WriteIndex(mustRef(t, host+"/multi@"+digest.String()), idx); err != nil {
		t.Fatalf("publishing index: %v", err)
	}

	withBase(obj, host+"/multi", digest.String())
	obj.Spec.Layers = []ociv1alpha1.Layer{removeLayer("noop", "/placeholder")}

	_, err = r.reconcileArtifact(context.Background(), obj)
	if err == nil {
		t.Fatal("a multi-architecture index was accepted")
	}
	if !strings.Contains(err.Error(), "multi-architecture index") {
		t.Fatalf("the error does not explain the problem: %v", err)
	}
	var te *recon.TerminalError
	if !asTerminalErr(err, &te) {
		t.Fatal("a multi-architecture index needs a spec change, so it must be terminal")
	}
}

// TestConfigSurfaceIsInTheInputHash — every config field lands in the image config and therefore
// in the output digest, so all of them must move the hash.
func TestConfigSurfaceIsInTheInputHash(t *testing.T) {
	layers := []oci.LayerInput{{Digest: "sha256:11", Unpack: oci.UnpackNone, Target: "/x"}}
	baseline := oci.InputHash(layers, oci.Config{}, "", nil)

	variants := map[string]oci.Config{
		"inherit":      {Inherit: true},
		"user":         {User: "1001"},
		"workingDir":   {WorkingDir: "/opt/kafka"},
		"stopSignal":   {StopSignal: "SIGTERM"},
		"exposedPorts": {ExposedPorts: []string{"9092/tcp"}},
		"volumes":      {Volumes: []string{"/data"}},
	}
	for name, cfg := range variants {
		t.Run(name, func(t *testing.T) {
			if oci.InputHash(layers, cfg, "", nil) == baseline {
				t.Fatalf("%s does not affect the input hash", name)
			}
		})
	}
}

func mustRef(t *testing.T, ref string) name.Reference {
	t.Helper()
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parsing %s: %v", ref, err)
	}
	return parsed
}
