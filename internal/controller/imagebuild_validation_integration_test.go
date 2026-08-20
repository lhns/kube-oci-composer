//go:build integration

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// ImageBuild's schema, against a real API server.
//
// Lives in internal/controller rather than internal/buildcontroller only because the envtest
// harness — the API server, the CRD directory, the client — is already set up here and standing a
// second one up would double the slowest part of the suite for no coverage.

// applyBuild creates an ImageBuild and returns the API server's error, if any.
func applyBuild(t *testing.T, name string, spec ociv1alpha1.ImageBuildSpec) error {
	t.Helper()
	obj := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
	err := k8s.Create(integrationCtx(t), obj)
	if err == nil {
		t.Cleanup(func() { _ = k8s.Delete(integrationCtx(t), obj) })
	}
	return err
}

func validBuildSpec() ociv1alpha1.ImageBuildSpec {
	return ociv1alpha1.ImageBuildSpec{
		Context:   ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "src"},
		Platforms: []string{"linux/amd64"},
		Push:      &ociv1alpha1.Push{Repository: "ghcr.io/me/app", Tags: []string{"v1"}},
	}
}

// TestIntegrationImageBuildValidSpecIsAccepted anchors the negative cases below.
func TestIntegrationImageBuildValidSpecIsAccepted(t *testing.T) {
	if err := applyBuild(t, "valid-build", validBuildSpec()); err != nil {
		t.Fatalf("a valid ImageBuild was rejected: %v", err)
	}
}

// TestIntegrationImageBuildDefaults — the defaults are part of the contract, and two of them
// carry real meaning: Sandbox is where reproducibility is lost, and a zero interval would make the
// controller requeue in a tight loop.
func TestIntegrationImageBuildDefaults(t *testing.T) {
	obj := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "build-defaults", Namespace: "default"},
		Spec:       validBuildSpec(),
	}
	if err := k8s.Create(integrationCtx(t), obj); err != nil {
		t.Fatalf("creating: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(integrationCtx(t), obj) })

	if obj.Spec.Interval == nil || obj.Spec.Interval.Duration.Hours() != 1 {
		t.Errorf("interval defaulted to %v, want 1h", obj.Spec.Interval)
	}
	if obj.Spec.Dockerfile != "Dockerfile" {
		t.Errorf("dockerfile defaulted to %q, want %q", obj.Spec.Dockerfile, "Dockerfile")
	}
	if obj.Spec.Network != "Sandbox" {
		t.Errorf("network defaulted to %q, want Sandbox", obj.Spec.Network)
	}
	if obj.Spec.Timeout == nil || obj.Spec.Timeout.Duration.Minutes() != 30 {
		t.Errorf("timeout defaulted to %v, want 30m", obj.Spec.Timeout)
	}
}

// TestIntegrationImageBuildPlatformsAreRequired — unlike ImageComposition's, where an unset list
// resolves to the base's platform. Neither of that field's defaults is available here, so the spec
// has to say it rather than the controller guessing.
func TestIntegrationImageBuildPlatformsAreRequired(t *testing.T) {
	spec := validBuildSpec()
	spec.Platforms = nil
	if err := applyBuild(t, "no-platforms", spec); err == nil {
		t.Fatal("an ImageBuild without platforms was accepted")
	}

	spec = validBuildSpec()
	spec.Platforms = []string{"not a platform"}
	if err := applyBuild(t, "bad-platform", spec); err == nil {
		t.Fatal("a malformed platform was accepted")
	}
}

// TestIntegrationImageBuildPushIsOptional — it was required, and is not any more.
//
// A build still always publishes to a registry: the Job runs in another pod and cannot reach the
// controller's loopback-only endpoint (ADR 0025). What changed is that WHICH registry can come from
// the operator's default instead of from every object, so a default install needs no spec to name a
// host at all.
//
// The schema therefore has to accept an ImageBuild with no push block. Whether one is REACHABLE is a
// runtime question the controller answers with Pending, because the answer depends on the
// controller's flags rather than on the object -- and stalling on operator configuration would wedge
// every build until someone edited specs that were never wrong.
func TestIntegrationImageBuildPushIsOptional(t *testing.T) {
	spec := validBuildSpec()
	spec.Push = nil
	if err := applyBuild(t, "no-push", spec); err != nil {
		t.Fatalf("an ImageBuild without push was refused: %v", err)
	}

	// And with a push block that names no repository, which is how an object opts into the default
	// registry while still setting tags or a conflict policy.
	spec = validBuildSpec()
	spec.Push = &ociv1alpha1.Push{Tags: []string{"v1"}}
	if err := applyBuild(t, "no-repository", spec); err != nil {
		t.Fatalf("an ImageBuild with push but no repository was refused: %v", err)
	}
}

// TestIntegrationImageBuildNetworkEnum — only two modes, and a typo must not silently mean
// Sandbox.
func TestIntegrationImageBuildNetworkEnum(t *testing.T) {
	for _, mode := range []string{"Sandbox", "None"} {
		spec := validBuildSpec()
		spec.Network = mode
		if err := applyBuild(t, "net-"+strings.ToLower(mode), spec); err != nil {
			t.Errorf("network %q was rejected: %v", mode, err)
		}
	}

	spec := validBuildSpec()
	spec.Network = "host"
	if err := applyBuild(t, "net-host", spec); err == nil {
		t.Fatal("network: host was accepted; there is no such mode")
	}
}

// TestIntegrationImageBuildArgNames — an ARG name that is not a shell identifier would be passed
// straight to BuildKit and fail there instead of here.
func TestIntegrationImageBuildArgNames(t *testing.T) {
	spec := validBuildSpec()
	spec.Args = []ociv1alpha1.BuildArg{{Name: "VALID_NAME", Value: "x"}}
	if err := applyBuild(t, "arg-ok", spec); err != nil {
		t.Errorf("a valid arg name was rejected: %v", err)
	}

	spec = validBuildSpec()
	spec.Args = []ociv1alpha1.BuildArg{{Name: "not-valid", Value: "x"}}
	if err := applyBuild(t, "arg-bad", spec); err == nil {
		t.Fatal("a malformed arg name was accepted")
	}
}

// TestIntegrationImageBuildAndCompositionCoexist — two kinds in one group, and the whole point of
// ADR 0004's split is that a reader can tell which promise they have by the kind alone.
func TestIntegrationImageBuildAndCompositionCoexist(t *testing.T) {
	if err := applyBuild(t, "coexist-build", validBuildSpec()); err != nil {
		t.Fatalf("ImageBuild rejected: %v", err)
	}
	if err := apply(t, "coexist-composition", ociv1alpha1.ImageCompositionSpec{
		Layers: []ociv1alpha1.Layer{fetchLayer("core")},
	}); err != nil {
		t.Fatalf("ImageComposition rejected: %v", err)
	}
}
