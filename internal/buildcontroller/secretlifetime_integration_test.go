//go:build integration

package buildcontroller

import (
	"context"
	"net/http/httptest"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// TestABuildsSecretsAreOwnedByItsJob: the per-build Secrets are named by input hash, so owned by
// the ImageBuild they would accumulate forever. Needs a real API server so the stored owner
// reference, UID included, is the one actually written. ADR 0050.
func TestABuildsSecretsAreOwnedByItsJob(t *testing.T) {
	ctx, k8s := integrationCtx(t)

	const ns = "secretlifetime"
	srv := buildableNamespace(t, ctx, k8s, ns)

	// The operator's credential, so pushSecretFor copies it. With the CA and inline Dockerfile
	// below, all four per-build Secrets exist.
	const opsNS = "default"
	if err := k8s.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "push-cred", Namespace: opsNS},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"registry.svc:5000":{"auth":"dTpw"}}}`)},
	}); err != nil {
		t.Fatalf("creating operator credential: %v", err)
	}

	obj := sampleBuild()
	obj.Namespace = ns
	obj.Spec.Context.SourceRef.Name = "src"
	// Inline, so dockerfileSecretFor writes one.
	obj.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{Inline: pinnedFrom}
	// The operator's own registry, so its credential applies.
	obj.Spec.Push = &ociv1alpha1.Push{Tags: []string{"v1"}}
	if err := k8s.Create(ctx, obj); err != nil {
		t.Fatalf("creating ImageBuild: %v", err)
	}

	cfg := sampleConfig()
	cfg.RegistryCA = []byte("-----BEGIN CERTIFICATE-----\nnot a real one\n-----END CERTIFICATE-----\n")
	cfg.ContextBaseURL = "http://builder.svc:8080"
	r := &ImageBuildReconciler{
		Client:     k8s,
		JobConfig:  cfg,
		HTTPClient: srv.Client(),
		Default: recon.DefaultRegistry{
			Host:       "registry.svc:5000",
			Namespace:  opsNS,
			SecretName: "push-cred",
		},
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: obj.Name},
	}); err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	var jobs batchv1.JobList
	if err := k8s.List(ctx, &jobs, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs.Items))
	}
	job := jobs.Items[0]

	var found int
	for _, name := range buildSecretNames(job.Name) {
		var sec corev1.Secret
		err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec)
		if err != nil {
			continue
		}
		found++
		owner := metav1.GetControllerOf(&sec)
		if owner == nil {
			t.Errorf("secret %s has no controller owner, so nothing will ever reclaim it", name)
			continue
		}
		if owner.Kind != "Job" || owner.Name != job.Name {
			t.Errorf("secret %s is owned by %s/%s, want Job/%s -- owned by the ImageBuild it "+
				"outlives every build the object ever runs", name, owner.Kind, owner.Name, job.Name)
		}
		if owner.UID != job.UID {
			t.Errorf("secret %s names the right Job with the wrong UID, so garbage collection "+
				"will not match it", name)
		}
	}
	// All four: push credential, registry CA, inline Dockerfile, and the context token.
	if found != 4 {
		t.Errorf("found %d of 4 per-build secrets; the fixture is not exercising every path", found)
	}
}

// TestAUserSuppliedPushSecretIsNeverAdopted: the user's own spec.push.secretRef must never be
// handed to the Job's garbage collection.
func TestAUserSuppliedPushSecretIsNeverAdopted(t *testing.T) {
	ctx, k8s := integrationCtx(t)

	const ns = "usersecret"
	srv := buildableNamespace(t, ctx, k8s, ns)

	obj := sampleBuild()
	obj.Namespace = ns
	obj.Spec.Context.SourceRef.Name = "src"
	obj.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{Inline: pinnedFrom}
	obj.Spec.Push = &ociv1alpha1.Push{
		Repository: "ghcr.io/me/app",
		Tags:       []string{"v1"},
		SecretRef:  &ociv1alpha1.LocalObjectReference{Name: "mine"},
	}
	if err := k8s.Create(ctx, obj); err != nil {
		t.Fatalf("creating ImageBuild: %v", err)
	}

	// The user's own credential, which must come out of the build untouched.
	mine := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: ns},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)},
	}
	if err := k8s.Create(ctx, mine); err != nil {
		t.Fatalf("creating the user secret: %v", err)
	}

	r := &ImageBuildReconciler{Client: k8s, JobConfig: sampleConfig(), HTTPClient: srv.Client()}
	if _, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: obj.Name},
	}); err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	var after corev1.Secret
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mine"}, &after); err != nil {
		t.Fatalf("the user's secret is gone: %v", err)
	}
	if owner := metav1.GetControllerOf(&after); owner != nil {
		t.Errorf("the user's own push credential was adopted by %s/%s; the Job's TTL would then "+
			"delete a Secret this controller never created", owner.Kind, owner.Name)
	}
}

// buildableNamespace creates a namespace with a Flux source an ImageBuild can build from, and
// returns the server standing in for source-controller. The stand-in CRD has no status
// subresource, so a plain update publishes the artifact.
func buildableNamespace(t *testing.T, ctx context.Context, k8s client.Client, ns string) *httptest.Server {
	t.Helper()
	if err := k8s.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	srv := contextServer(t, contextTarball(t, "", pinnedFrom))
	src := fluxSource(ns, "src", srv.URL, "sha256:ctx", "main@sha1:abcd")
	if err := k8s.Create(ctx, src); err != nil {
		t.Fatalf("creating source: %v", err)
	}
	if err := k8s.Update(ctx, src); err != nil {
		t.Fatalf("writing source status: %v", err)
	}
	return srv
}
