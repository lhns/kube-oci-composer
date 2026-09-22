package buildcontroller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// A Dockerfile that does not live in the build context. The shared property: the pod builds the
// exact bytes the controller hashed and checked.

func dockerfileConfigMap(name, key, content string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		Data:       map[string]string{key: content},
	}
}

func usingConfigMapDockerfile(name, key string) func(*ociv1alpha1.ImageBuild) {
	return func(o *ociv1alpha1.ImageBuild) {
		o.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{
			ConfigMapRef: &ociv1alpha1.ConfigMapKeyReference{Name: name, Key: key},
		}
	}
}

// TestADockerfileCanComeFromAConfigMap: a platform team owns the recipe, an application team the
// ImageBuild.
func TestADockerfileCanComeFromAConfigMap(t *testing.T) {
	obj := buildOf(t, usingConfigMapDockerfile("recipes", "Dockerfile"))
	r := harness(t, "", obj, dockerfileConfigMap("recipes", "Dockerfile", pinnedFrom))

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	jobs := jobsIn(t, r, obj.Namespace)
	if len(jobs) != 1 {
		t.Fatalf("want one Job, got %d", len(jobs))
	}

	// Through a controller-owned Secret holding the checked bytes, never the user's ConfigMap,
	// which the kubelet would re-read at pod start.
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: obj.Namespace, Name: jobs[0].Name + "-dockerfile"}
	if err := r.Get(t.Context(), key, &sec); err != nil {
		t.Fatalf("the checked Dockerfile was not materialised: %v", err)
	}
	if got := string(sec.Data["Dockerfile"]); got != pinnedFrom {
		t.Errorf("the projected Dockerfile is %q, not the bytes that were checked", got)
	}
	if len(sec.OwnerReferences) == 0 {
		t.Error("the Dockerfile Secret has no owner, so it outlives the build it belongs to")
	}
}

// TestAMissingConfigMapWaitsRatherThanStalling: creating the ConfigMap is the fix, and applying
// both in one commit can race.
func TestAMissingConfigMapWaitsRatherThanStalling(t *testing.T) {
	obj := buildOf(t, usingConfigMapDockerfile("absent", "Dockerfile"))
	r := harness(t, "", obj)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c := conditionOf(reload(t, r, obj), ociv1alpha1.StalledCondition); c != nil {
		t.Errorf("a missing ConfigMap stalled the object: %+v", c)
	}
	if res.RequeueAfter == 0 {
		t.Error("no retry scheduled, so creating the ConfigMap would never be noticed")
	}
	if jobs := jobsIn(t, r, obj.Namespace); len(jobs) != 0 {
		t.Errorf("a Job was created with no Dockerfile to build: %d", len(jobs))
	}
}

// TestAMissingKeyWaitsToo: adding the key raises no generation change here.
func TestAMissingKeyWaitsToo(t *testing.T) {
	obj := buildOf(t, usingConfigMapDockerfile("recipes", "Dockerfile.prod"))
	r := harness(t, "", obj, dockerfileConfigMap("recipes", "Dockerfile", pinnedFrom))

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c := conditionOf(reload(t, r, obj), ociv1alpha1.StalledCondition); c != nil {
		t.Errorf("a missing key stalled the object: %+v", c)
	}
}

// TestEditingTheConfigMapMovesTheInputHash: the content is outside the context digest, so it
// must be hashed itself.
func TestEditingTheConfigMapMovesTheInputHash(t *testing.T) {
	obj := buildOf(t, usingConfigMapDockerfile("recipes", "Dockerfile"))
	r := harness(t, "", obj, dockerfileConfigMap("recipes", "Dockerfile", pinnedFrom))

	before, _, err := r.resolveInputs(t.Context(), obj)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}

	var cm corev1.ConfigMap
	if err := r.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "recipes"}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["Dockerfile"] = pinnedFrom + "RUN echo changed\n"
	if err := r.Update(t.Context(), &cm); err != nil {
		t.Fatal(err)
	}

	after, _, err := r.resolveInputs(t.Context(), obj)
	if err != nil {
		t.Fatalf("resolving after the edit: %v", err)
	}
	if before.Hash() == after.Hash() {
		t.Error("editing the ConfigMap did not move the input hash, so the build would never rerun")
	}
}

// TestRenamingTheConfigMapAloneDoesNotRebuild: content is hashed, not identity.
func TestRenamingTheConfigMapAloneDoesNotRebuild(t *testing.T) {
	first := buildOf(t, usingConfigMapDockerfile("recipes", "Dockerfile"))
	r := harness(t, "", first,
		dockerfileConfigMap("recipes", "Dockerfile", pinnedFrom),
		dockerfileConfigMap("recipes-copy", "Dockerfile", pinnedFrom))

	before, _, err := r.resolveInputs(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}

	second := first.DeepCopy()
	second.Spec.Dockerfile.ConfigMapRef.Name = "recipes-copy"
	after, _, err := r.resolveInputs(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() != after.Hash() {
		t.Error("the same Dockerfile under a different ConfigMap name changed the hash; the name " +
			"is not the content, and rebuilding for it is wasted work")
	}
}

// TestAConfigMapDockerfileCannotBePinned: a ConfigMap is mutable, so --require-pinned-sources
// refuses it. The context is pinned so the ConfigMap is the only possible reason, and the message
// is asserted so a refusal for another reason does not pass.
func TestAConfigMapDockerfileCannotBePinned(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) {
		usingConfigMapDockerfile("recipes", "Dockerfile")(o)
		o.Spec.Context.SourceRef.Revision = "main@sha1:abcd"
	})
	r := harness(t, "", obj, dockerfileConfigMap("recipes", "Dockerfile", pinnedFrom))
	r.RequirePinnedSources = true

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	c := conditionOf(reload(t, r, obj), ociv1alpha1.StalledCondition)
	if c == nil {
		t.Fatal("a ConfigMap Dockerfile was accepted under --require-pinned-sources, so the flag " +
			"does not mean what it says")
	}
	if !strings.Contains(c.Message, "configMapRef") {
		t.Errorf("stalled for the wrong reason: %q", c.Message)
	}
}

// TestAConfigMapEditEnqueuesOnlyTheBuildsThatReadIt covers the watch.
func TestAConfigMapEditEnqueuesOnlyTheBuildsThatReadIt(t *testing.T) {
	reader := buildOf(t, usingConfigMapDockerfile("recipes", "Dockerfile"))
	other := buildOf(t, func(o *ociv1alpha1.ImageBuild) { o.Name = "unrelated" })
	r := harness(t, pinnedFrom, reader, other,
		dockerfileConfigMap("recipes", "Dockerfile", pinnedFrom))

	reqs := r.buildsForConfigMap(t.Context(), dockerfileConfigMap("recipes", "Dockerfile", pinnedFrom))
	if len(reqs) != 1 {
		t.Fatalf("want exactly the one build that reads it, got %d: %+v", len(reqs), reqs)
	}
	if reqs[0].Name != reader.Name {
		t.Errorf("enqueued %q, want %q", reqs[0].Name, reader.Name)
	}

	// A same-named ConfigMap in another namespace is unrelated (I4).
	elsewhere := dockerfileConfigMap("recipes", "Dockerfile", pinnedFrom)
	elsewhere.Namespace = "team-b"
	if got := r.buildsForConfigMap(t.Context(), elsewhere); len(got) != 0 {
		t.Errorf("a ConfigMap in another namespace enqueued %d builds", len(got))
	}
}

// TestAnImageContextDefersTheFromCheckToTheFetcher: the controller cannot read an image without
// registry credentials for arbitrary repositories, so the fetcher runs the FROM check instead.
func TestAnImageContextDefersTheFromCheckToTheFetcher(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) {
		o.Spec.Context = &ociv1alpha1.BuildContext{Image: &ociv1alpha1.ImageSource{
			Ref: "ghcr.io/me/ctx@sha256:" + strings.Repeat("a", 64),
		}}
	})
	// The harness serves an unpinned Dockerfile: if the controller read it, no Job would exist.
	r := harness(t, "FROM golang:1.26\n", obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	jobs := jobsIn(t, r, obj.Namespace)
	if len(jobs) != 1 {
		t.Fatalf("want one Job, got %d", len(jobs))
	}

	// The fetcher must be told to run the check, or the guard is gone rather than moved.
	args := strings.Join(jobs[0].Spec.Template.Spec.InitContainers[0].Args, " ")
	if !strings.Contains(args, "--dockerfile=Dockerfile") {
		t.Errorf("the fetcher was not told which Dockerfile to check, so an unpinned FROM would "+
			"reach BuildKit\ngot: %s", args)
	}
	if !strings.Contains(args, "--kind=image") {
		t.Errorf("the fetcher was not told this is an image context\ngot: %s", args)
	}
}

// TestAProjectedDockerfileIsNotCheckedTwice: it is not in the tree, so the fetcher must not look
// for it there.
func TestAProjectedDockerfileIsNotCheckedTwice(t *testing.T) {
	obj := buildOf(t, func(o *ociv1alpha1.ImageBuild) {
		o.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{
			Inline: "FROM scratch@sha256:" + strings.Repeat("a", 64) + "\n",
		}
	})
	r := harness(t, "", obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	jobs := jobsIn(t, r, obj.Namespace)
	if len(jobs) != 1 {
		t.Fatalf("want one Job, got %d", len(jobs))
	}
	args := strings.Join(jobs[0].Spec.Template.Spec.InitContainers[0].Args, " ")
	if strings.Contains(args, "--dockerfile=") {
		t.Errorf("the fetcher was pointed at a Dockerfile that is not in the context\ngot: %s", args)
	}
}
