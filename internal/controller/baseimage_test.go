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

func imageLayer(name, repository, digest string) ociv1alpha1.Layer {
	return ociv1alpha1.Layer{
		Name:   name,
		Image:  &ociv1alpha1.ImageSource{Repository: repository},
		Digest: digest,
	}
}

// TestBaseImageLayersAreContributed is the Kafka-plus-plugins case: an upstream image, with our
// content added on top, producing something runnable rather than a bundle to mount.
func TestBaseImageLayersAreContributed(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"plugins/core.jar": "jar"})

	obj := composition("layered")
	r, host := servingReconciler(t, obj)

	baseRepo, baseDigest := publishBaseImage(t, host, "kafka", 3, nil)
	obj.Spec.Layers = []ociv1alpha1.Layer{
		imageLayer("base", baseRepo, baseDigest),
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

// TestLayerOrderPlacesTheBaseWhereItIsDeclared — an image entry is not implicitly first. Putting
// one second must put its layers second. See ADR 0003.
func TestLayerOrderPlacesTheBaseWhereItIsDeclared(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"a.txt": "a"})

	obj := composition("order")
	r, host := servingReconciler(t, obj)
	baseRepo, baseDigest := publishBaseImage(t, host, "base", 2, nil)

	first := func() string {
		obj.Spec.Layers = []ociv1alpha1.Layer{
			imageLayer("base", baseRepo, baseDigest),
			urlLayer("files", origin.url, origin.digest, "/files"),
		}
		obj.Status = ociv1alpha1.ImageCompositionStatus{}
		return build(t, r, obj, "base first").Digest
	}()

	second := func() string {
		obj.Spec.Layers = []ociv1alpha1.Layer{
			urlLayer("files", origin.url, origin.digest, "/files"),
			imageLayer("base", baseRepo, baseDigest),
		}
		obj.Status = ociv1alpha1.ImageCompositionStatus{}
		return build(t, r, obj, "base second").Digest
	}()

	if first == second {
		t.Fatal("moving the image entry did not change the output; order is not being honoured")
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

	obj.Spec.Layers = []ociv1alpha1.Layer{
		imageLayer("base", baseRepo, baseDigest),
		urlLayer("plugins", origin.url, origin.digest, "/plugins"),
	}
	obj.Spec.Config = &ociv1alpha1.ImageConfig{From: "base"}

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
	obj.Spec.Layers = []ociv1alpha1.Layer{imageLayer("base", baseRepo, baseDigest)}

	art := build(t, r, obj, "compose without config.from")

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

// TestConfigFromMustNameAnImage — naming a url layer is a spec mistake, and silently ignoring it
// would leave the user with a non-runnable image and no explanation.
func TestConfigFromMustNameAnImage(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"a": "1"})
	obj := composition("badfrom", urlLayer("files", origin.url, origin.digest, "/files"))
	obj.Spec.Config = &ociv1alpha1.ImageConfig{From: "files"}
	r, _ := servingReconciler(t, obj)

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil || !strings.Contains(err.Error(), "not an image source") {
		t.Fatalf("expected a clear error, got %v", err)
	}
}

// TestConfigFromMustExist — a typo must not silently produce an empty config.
func TestConfigFromMustExist(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"a": "1"})
	obj := composition("missingfrom", urlLayer("files", origin.url, origin.digest, "/files"))
	obj.Spec.Config = &ociv1alpha1.ImageConfig{From: "nope"}
	r, _ := servingReconciler(t, obj)

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected a clear error, got %v", err)
	}
}

// TestBaseImageIsNotPulledWhenNothingChanged — the short-circuit must cover image entries too, or
// every interval would mean a registry round trip per base image.
func TestBaseImageIsNotPulledWhenNothingChanged(t *testing.T) {
	obj := composition("steady-base")
	r, host := servingReconciler(t, obj)
	baseRepo, baseDigest := publishBaseImage(t, host, "kafka", 1, nil)
	obj.Spec.Layers = []ociv1alpha1.Layer{imageLayer("base", baseRepo, baseDigest)}

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

	obj.Spec.Layers = []ociv1alpha1.Layer{imageLayer("base", host+"/multi", digest.String())}

	_, err = r.reconcileArtifact(context.Background(), obj)
	if err == nil {
		t.Fatal("a multi-architecture index was accepted")
	}
	if !strings.Contains(err.Error(), "multi-architecture index") {
		t.Fatalf("the error does not explain the problem: %v", err)
	}
	var te *terminalError
	if !asTerminalErr(err, &te) {
		t.Fatal("a multi-architecture index needs a spec change, so it must be terminal")
	}
}

// TestConfigFromIsInTheInputHash — it selects which config is inherited, so it changes the output
// and must move the hash. It was absent while From was unimplemented.
func TestConfigFromIsInTheInputHash(t *testing.T) {
	layers := []oci.LayerInput{{Digest: "sha256:11", Unpack: oci.UnpackNone, Target: "/x"}}

	if oci.InputHash(layers, oci.Config{}) == oci.InputHash(layers, oci.Config{From: "base"}) {
		t.Fatal("config.from does not affect the input hash")
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
