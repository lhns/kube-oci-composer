//go:build integration

package buildcontroller

import (
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

// TestABuildsSecretsAreOwnedByItsJob is the leak, asserted against a real API server.
//
// The four per-build Secrets were owner-referenced to the ImageBuild, which a GitOps layer never
// deletes, and their names carry the input hash -- so every revision added four more rather than
// replacing them. A ten-day-old install reported 42 of 63 Secrets in one namespace being garbage,
// the largest Secret consumer in the cluster.
//
// This needs a real API server rather than a fake client: ownership is only meaningful if the
// reference the apiserver stores is the one we think we wrote, UID included. ADR 0050.
func TestABuildsSecretsAreOwnedByItsJob(t *testing.T) {
	ctx, k8s := integrationCtx(t)

	const ns = "secretlifetime"
	if err := k8s.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	// The operator's credential, so pushSecretFor copies it into the object's namespace. Both the
	// CA and an inline Dockerfile are set too, so all four Secrets exist in one build rather than
	// leaving the TLS and inline paths untested.
	const opsNS = "default"
	if err := k8s.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "push-cred", Namespace: opsNS},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"registry.svc:5000":{"auth":"dTpw"}}}`)},
	}); err != nil {
		t.Fatalf("creating operator credential: %v", err)
	}

	srv := contextServer(t, contextTarball(t, "", pinnedFrom))
	src := fluxSource(ns, "src", srv.URL, "sha256:ctx", "main@sha1:abcd")
	if err := k8s.Create(ctx, src); err != nil {
		t.Fatalf("creating source: %v", err)
	}
	if err := k8s.Update(ctx, src); err != nil {
		t.Fatalf("writing source status: %v", err)
	}

	obj := sampleBuild()
	obj.Namespace = ns
	obj.Spec.Context.SourceRef.Name = "src"
	// Inline, so dockerfileSecretFor writes one. Digest-pinned, as every FROM must be.
	obj.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{Inline: pinnedFrom}
	// Published to the operator's own registry, so the operator's credential applies.
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

// TestAUserSuppliedPushSecretIsNeverAdopted is the guard on the dangerous half.
//
// pushSecretFor returns the OBJECT's own Secret when spec.push.secretRef is set -- one this
// controller neither created nor owns. Adopting whatever name came back would hand a user's
// credential to the Job's garbage collection and delete it an hour after the build finished.
func TestAUserSuppliedPushSecretIsNeverAdopted(t *testing.T) {
	ctx, k8s := integrationCtx(t)

	const ns = "usersecret"
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

	// The user's own credential. Named nothing like ours, carrying none of our labels, owned by
	// nobody -- and it must come out of the build exactly as it went in.
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
