package buildcontroller

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// checkTagConflict applies spec.push.onConflict BEFORE a Job is created.
//
// Until this existed the field was INERT on this kind: nothing in this package read `immutable`,
// and BuildKit pushed `type=image,push=true` over whatever the tag held. The CRD advertised a
// guarantee that was enforced nowhere, which is worse than not offering it — an operator who set
// `immutable: true` believed a tag could not be remeaned, and it could.
//
// The check runs before the Job rather than after, and that ordering is the whole point. BuildKit
// pushes from inside the Job, so by the time this controller sees a result the tag has already
// moved; there is no undo. Checking first is what makes Fail actually refuse, and it also saves the
// build entirely under Keep.
//
// Returns whether the caller should stop, and the divergence to record if so.
func (r *ImageBuildReconciler) checkTagConflict(
	ctx context.Context, obj *ociv1alpha1.ImageBuild,
) (stop bool, conflict *ociv1alpha1.TagConflictStatus, err error) {
	p := obj.Spec.Push
	repo := r.repositoryFor(obj)
	if repo == "" {
		return false, nil, recon.Pending(
			"this build names no push.repository, and no default registry is configured")
	}
	policy := p.ResolveConflictPolicy()
	if policy == ociv1alpha1.ConflictOverwrite {
		// Nothing to ask the registry. Skipping the round trip also means the permissive policy
		// keeps working when the registry is unreachable for reads but writable for pushes.
		return false, nil, nil
	}

	tags, err := recon.EffectiveTags(p.GetTags(), p.GetRef())
	if err != nil {
		return false, nil, err
	}
	if len(tags) == 0 {
		// Digest-only publication cannot collide: the name IS the content.
		return false, nil, nil
	}

	opts, err := r.remoteOptions(ctx, obj)
	if err != nil {
		return false, nil, err
	}
	var refOpts []name.Option
	if recon.InsecureHost(repo, r.JobConfig.InsecureRegistries) {
		refOpts = append(refOpts, name.Insecure)
	}

	published, err := recon.ResolvePublished(repo, tags, obj.Status.Artifact, refOpts, opts)
	if err != nil {
		return false, nil, err
	}

	// What this build WILL produce is unknown -- that is the whole difference from the composer,
	// whose output is a function of its spec (ADR 0025). So the question is not "does the tag hold
	// something else than what we are about to push", which is unanswerable here, but "does the tag
	// already hold something". The digest recorded in status is the one value that is legitimately
	// ours, so a tag pointing at it is not a conflict.
	ours := ""
	if obj.Status.Artifact != nil {
		ours = obj.Status.Artifact.Digest
	}
	tag, current := published.Conflicts(tags, ours)
	if tag == "" {
		return false, nil, nil
	}

	switch policy {
	case ociv1alpha1.ConflictFail:
		return false, nil, recon.Terminal(
			"tag %s already resolves to %s and this build would replace it with different content; "+
				"change the tag, or set onConflict: Overwrite if it is meant to move, or "+
				"onConflict: Keep to leave it alone", tag, current)

	case ociv1alpha1.ConflictKeep:
		now := metav1.Now()
		return true, &ociv1alpha1.TagConflictStatus{
			Tag:      tag,
			Existing: current,
			// Nothing was built, so nothing was dropped -- and saying otherwise would invent a
			// digest that never existed. The empty value is the honest one, and it is also the
			// difference from the composer, which produces its artifact before it can conflict.
			ObservedAt: &now,
		}, nil
	default:
		// Unreachable: Overwrite returned above and CEL refuses anything outside the enum. Refusing
		// rather than falling through means a value added to the enum without a branch here fails
		// loudly instead of silently overwriting a tag.
		return false, nil, recon.Terminal("unknown onConflict policy %q", policy)
	}
}

// remoteOptions builds registry auth from spec.push.secretRef, the same Secret the Job is given.
//
// Credentials are read from a Secret and never from the spec, and the controller only ever GETs the
// one it was pointed at -- it has no list or watch on secrets, so it cannot enumerate a namespace's
// credentials even in principle.
func (r *ImageBuildReconciler) remoteOptions(
	ctx context.Context, obj *ociv1alpha1.ImageBuild,
) ([]remote.Option, error) {
	opts := []remote.Option{remote.WithContext(ctx)}

	var ownRef string
	if p := obj.Spec.Push; p != nil && p.SecretRef != nil {
		ownRef = p.SecretRef.Name
	}
	// The operator's credential goes to the operator's registry and nowhere else, whether or not
	// this object named the path itself. See recon.DefaultRegistry.CredentialFor.
	name, namespace := r.Default.CredentialFor(obj.Namespace, ownRef, r.repositoryFor(obj))
	if name == "" {
		return append(opts, remote.WithAuth(authn.Anonymous)), nil
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Waits rather than stalls: the Secret may be on its way from SOPS or a Kustomization
			// applied moments later, and its creation raises no event on this object.
			return nil, recon.Pending("push secret %s not found yet", key)
		}
		return nil, fmt.Errorf("reading push secret %s: %w", key, err)
	}

	kc, err := recon.KeychainFromSecret(&secret)
	if err != nil {
		return nil, recon.Pending("push secret %s is unusable: %v", key, err)
	}
	return append(opts, remote.WithAuthFromKeychain(kc)), nil
}

// cacheAvailable reports whether this object's build cache reference resolves.
//
// Asked because BuildKit treats a cache reference it cannot resolve as a fatal error rather than a
// warning, so importing one that does not exist yet fails the build -- see buildctlArgs. Answering
// it costs one HEAD on a path that is about to run a build anyway.
//
// Any failure answers "no". A registry that cannot be reached, a malformed reference, an
// unreadable secret: none of them are reasons to fail a build over a cache, and the worst outcome
// of a wrong "no" is that this build repopulates a cache that was already there.
func (r *ImageBuildReconciler) cacheAvailable(ctx context.Context, obj *ociv1alpha1.ImageBuild) bool {
	cacheRef := cacheRefFor(obj, r.repositoryFor(obj))
	if cacheRef == "" {
		return false
	}

	opts, err := r.remoteOptions(ctx, obj)
	if err != nil {
		return false
	}
	var refOpts []name.Option
	if recon.InsecureHost(cacheRef, r.JobConfig.InsecureRegistries) {
		refOpts = append(refOpts, name.Insecure)
	}

	ref, err := name.ParseReference(cacheRef, refOpts...)
	if err != nil {
		return false
	}
	_, err = remote.Head(ref, opts...)
	return err == nil
}

// usesDefaultRepository reports whether this build publishes to the operator's default registry
// rather than to one its own spec named.
func usesDefaultRepository(obj *ociv1alpha1.ImageBuild) bool {
	return obj.Spec.Push == nil || obj.Spec.Push.Repository == ""
}

// repositoryFor is the ONE place that resolves where a build publishes.
//
// Everything that needs the repository goes through here -- the push target, the tag-conflict
// check, the build cache reference, the retention refresh. Resolving the default in some of those
// and not others would push to one place and then keep a different one alive, which is the kind of
// mismatch that only shows up a retention window later.
func (r *ImageBuildReconciler) repositoryFor(obj *ociv1alpha1.ImageBuild) string {
	if !usesDefaultRepository(obj) {
		return obj.Spec.Push.Repository
	}
	if !r.Default.Configured() {
		return ""
	}
	return r.Default.RepositoryFor(obj.Namespace, obj.Name)
}

// pushSecretFor returns the name of the Secret the build pod mounts as its registry credential,
// creating a short-lived copy of the operator's when the object has none of its own.
//
// A pod can only mount Secrets from its own namespace, and the build must run in the object's
// namespace: it mounts that namespace's build secrets and executes that namespace's code. So the
// operator's credential, which lives in the CONTROLLER's namespace, cannot be mounted directly.
//
// The copy is owned by the ImageBuild and named after the Job, so it is garbage-collected when the
// object goes and is replaced rather than accumulated when the inputs change. That bounds the
// exposure to roughly the length of a build instead of forever, which is the whole reason it is a
// copy and not a permanent per-namespace Secret.
//
// It does NOT eliminate the exposure, and pretending otherwise would be worse than not doing it:
// while a build runs, anyone who can read Secrets in that namespace can read the operator's registry
// credential. What makes that tolerable is that the namespace can already push arbitrary content to
// that registry through an ImageBuild -- the credential lets it do directly what it could already do
// indirectly.
func (r *ImageBuildReconciler) pushSecretFor(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, jobName string,
) (string, error) {
	if p := obj.Spec.Push; p != nil && p.SecretRef != nil {
		return p.SecretRef.Name, nil
	}

	repo := r.repositoryFor(obj)
	if r.Default.SecretName == "" || !r.Default.Owns(repo) {
		// Either there is no operator credential, or this build publishes somewhere the operator's
		// credential has no business going. Push anonymously; the registry decides.
		return "", nil
	}

	var source corev1.Secret
	key := types.NamespacedName{Namespace: r.Default.Namespace, Name: r.Default.SecretName}
	if err := r.Get(ctx, key, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return "", recon.Pending("default push secret %s not found yet", key)
		}
		return "", fmt.Errorf("reading default push secret %s: %w", key, err)
	}

	copied := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-push",
			Namespace: obj.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kube-oci-builder"},
			Annotations: map[string]string{
				"oci.lhns.de/description": "Short-lived copy of the operator's registry credential, " +
					"mounted by this build's Job. Deleted with the build.",
			},
		},
		Type: source.Type,
		Data: source.Data,
	}
	if err := ctrl.SetControllerReference(obj, copied, r.Scheme()); err != nil {
		return "", fmt.Errorf("setting owner on the push credential: %w", err)
	}

	if err := r.Create(ctx, copied); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating the push credential: %w", err)
		}
		// Already there from an earlier attempt at this same build. Update it, so a rotated
		// operator password reaches the build rather than the build failing on a stale one.
		existing := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(copied), existing); err != nil {
			return "", fmt.Errorf("reading the existing push credential: %w", err)
		}
		existing.Data = source.Data
		if err := r.Update(ctx, existing); err != nil {
			return "", fmt.Errorf("refreshing the push credential: %w", err)
		}
	}
	return copied.Name, nil
}
