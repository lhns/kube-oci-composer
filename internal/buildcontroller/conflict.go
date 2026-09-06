package buildcontroller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
	if r.repositoryFor(obj) == "" {
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

	reg, err := r.registryFor(ctx, obj)
	if err != nil {
		return false, nil, err
	}

	published, err := recon.ResolvePublished(reg.repo, tags, obj.Status.Artifact, reg.refOpts, reg.opts)
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
	if r.Transport != nil {
		opts = append(opts, remote.WithTransport(r.Transport))
	}

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
					"mounted by this build's Job. Owned by this build's Job and deleted with it.",
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

// registryCASecretFor puts the operator's registry CA where a build Job can mount it.
//
// Same shape and same reasoning as pushSecretFor: the Job runs in the OBJECT's namespace and a pod
// can only mount Secrets from its own, so the material is copied there. Created owned by the
// ImageBuild because the Job does not exist yet, then re-owned by the Job -- see adoptBuildSecrets.
//
// A SEPARATE object rather than an extra key on the copied push credential, and that is not
// tidiness. pushSecretFor returns early when the object has its own `spec.push.secretRef` — an
// object may legitimately use its own credential to push to a path in the operator's registry
// (recon.DefaultRegistry.CredentialFor permits exactly that). Riding the CA on the copy would give
// those builds no CA at all and a TLS failure that looks nothing like a credential problem.
//
// A Secret rather than a ConfigMap for something that is not secret: the builder already holds
// get/create/update on secrets cluster-wide (see the RBAC markers above). ConfigMaps would mean a
// new verb on a new resource in every tenant namespace, which is a real cost for a cosmetic gain.
func (r *ImageBuildReconciler) registryCASecretFor(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, jobName string,
) (string, error) {
	if len(r.JobConfig.RegistryCA) == 0 {
		return "", nil
	}

	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-registry-ca",
			Namespace: obj.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kube-oci-builder"},
			Annotations: map[string]string{
				"oci.lhns.de/description": "The registry CA this build's Job trusts. " +
					"Not secret; a Secret only because the builder already has permission to " +
					"write Secrets here. Owned by this build's Job and deleted with it.",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"ca.crt": r.JobConfig.RegistryCA},
	}
	if err := ctrl.SetControllerReference(obj, ca, r.Scheme()); err != nil {
		return "", fmt.Errorf("setting owner on the registry CA: %w", err)
	}

	if err := r.Create(ctx, ca); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating the registry CA: %w", err)
		}
		// Update rather than leave it: a rotated CA has to reach a retried build, or the retry
		// fails for a reason that was already fixed.
		existing := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(ca), existing); err != nil {
			return "", fmt.Errorf("reading the existing registry CA: %w", err)
		}
		existing.Data = ca.Data
		if err := r.Update(ctx, existing); err != nil {
			return "", fmt.Errorf("refreshing the registry CA: %w", err)
		}
	}
	return ca.Name, nil
}

// dockerfileSecretFor puts a Dockerfile that does not live in the build context where the Job can
// mount it.
//
// The bytes are the ones the controller has already hashed and run CheckPinnedBases over. That is
// the point of copying rather than projecting the user's object directly: the kubelet resolves a
// volume at pod start, reading whatever the source says THEN, not what the controller checked a
// moment earlier. An edit landing in that window — seconds to minutes of scheduling and image pull
// — would build a Dockerfile that was never checked, which is a complete bypass of the only content
// guard this controller has, reachable with `update` on the source object.
//
// Immutable, and safely so: jobName derives from the input hash, and the Dockerfile's content is
// part of that hash, so the content is a function of the name. Different bytes are a different Job.
// That is also why there is no update path here, unlike registryCASecretFor.
//
// A Secret rather than a ConfigMap for a Dockerfile, which is not secret, for the reason given
// above registryCASecretFor: the builder already holds get/create/update on secrets cluster-wide,
// and ConfigMaps would mean a new write verb on a new resource in every tenant namespace.
func (r *ImageBuildReconciler) dockerfileSecretFor(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, jobName string, content []byte,
) (string, error) {
	if len(content) == 0 {
		return "", nil
	}

	df := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-dockerfile",
			Namespace: obj.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kube-oci-builder"},
			Annotations: map[string]string{
				"oci.lhns.de/description": "The Dockerfile this build runs, as checked by the " +
					"controller. Not secret; a Secret only because the builder already has " +
					"permission to write Secrets here. Owned by this build's Job and deleted with it.",
			},
		},
		Type:      corev1.SecretTypeOpaque,
		Immutable: ptr.To(true),
		Data:      map[string][]byte{dockerfileKey: content},
	}
	if err := ctrl.SetControllerReference(obj, df, r.Scheme()); err != nil {
		return "", fmt.Errorf("setting owner on the Dockerfile: %w", err)
	}

	if err := r.Create(ctx, df); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating the Dockerfile: %w", err)
		}
		// Already there, and because the name carries the input hash it already holds these bytes.
		// Nothing to refresh, and an Update would be refused by Immutable anyway.
	}
	return df.Name, nil
}

// contextTokenFor mints the bearer token the build pod uses to fetch its own context.
//
// Random and per-build, in a Secret named for the Job -- so the name carries the input hash, and a
// token cannot open a build other than the one it was minted for. Owner-referenced, so it is
// deleted with the object; Immutable, because the pod reads it once at startup and nothing may
// change what it is under a running build.
//
// On AlreadyExists the STORED value is returned rather than the freshly generated one. A retry or a
// leader change must hand the pod the token the endpoint will actually accept.
func (r *ImageBuildReconciler) contextTokenFor(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, jobName string,
) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a context token: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      contextSecretName(jobName),
			Namespace: obj.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kube-oci-builder"},
			Annotations: map[string]string{
				"oci.lhns.de/description": "Lets this build fetch its own source through the " +
					"builder, so the build pod never reaches source-controller. Owned by this build's Job and deleted with it.",
			},
		},
		Type:      corev1.SecretTypeOpaque,
		Immutable: ptr.To(true),
		Data:      map[string][]byte{contextTokenKey: []byte(hex.EncodeToString(raw))},
	}
	if err := ctrl.SetControllerReference(obj, secret, r.Scheme()); err != nil {
		return "", fmt.Errorf("setting owner on the context token: %w", err)
	}

	if err := r.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating the context token: %w", err)
		}
	}
	return secret.Name, nil
}

// buildSecretNames are the Secrets this controller creates for one Job, derived rather than
// collected.
//
// Derived on purpose. pushSecretFor returns the OBJECT's own secret when spec.push.secretRef is set
// -- a Secret this controller neither made nor owns -- so adopting whatever name came back would
// hand a user's credential to the Job's garbage collection and delete it an hour after the build.
func buildSecretNames(jobName string) []string {
	return []string{
		jobName + "-push",
		jobName + "-registry-ca",
		jobName + "-dockerfile",
		contextSecretName(jobName),
	}
}

// adoptBuildSecrets hands each per-build Secret's lifetime to the Job that mounts it.
//
// They are created owned by the ImageBuild, because the Job does not exist yet and an owner must.
// Left that way they outlive every build the object ever runs: Kubernetes reclaims a dependent only
// when its OWNER goes, the ImageBuild is a GitOps object that does not, and their names carry the
// input hash so each revision adds four more rather than replacing them. A ten-day-old install
// reported 42 of 63 Secrets in one namespace being garbage.
//
// The Job already carries TTLSecondsAfterFinished and is deleted outright on retry, so owning them
// from it is the whole fix -- no pruning loop, no reconstructing which are dead, and nothing that
// could delete a running build's credentials. It also makes their annotation true.
//
// Deliberately NOT a list-and-prune sweep, which would need `list` on Secrets cluster-wide; this
// needs only `update`, which the builder already has. ADR 0050.
func (r *ImageBuildReconciler) adoptBuildSecrets(ctx context.Context, obj *ociv1alpha1.ImageBuild, job *batchv1.Job) {
	log := logf.FromContext(ctx)
	for _, name := range buildSecretNames(job.Name) {
		var sec corev1.Secret
		key := types.NamespacedName{Namespace: job.Namespace, Name: name}
		if err := r.Get(ctx, key, &sec); err != nil {
			// Most of these do not exist for any given build -- no TLS, no inline Dockerfile, no
			// proxied context -- so absence is the ordinary case and not worth a line.
			if !apierrors.IsNotFound(err) {
				log.Error(err, "reading a build secret to re-own it", "secret", key)
			}
			continue
		}
		// Three guards, and the point of all three is that this can never touch a Secret the
		// controller did not create: the name is one it generates, the label is one it sets, and it
		// is currently owned by this very object.
		if sec.Labels["app.kubernetes.io/managed-by"] != "kube-oci-builder" {
			continue
		}
		if !metav1.IsControlledBy(&sec, obj) {
			continue
		}
		// Not fatal at any step. The Job is already running and a build must not fail over its own
		// housekeeping; the outcome is the previous behaviour, which is a leak and not an outage.
		sec.OwnerReferences = nil
		err := ctrl.SetControllerReference(job, &sec, r.Scheme())
		if err == nil {
			err = r.Update(ctx, &sec)
		}
		if err != nil {
			log.Error(err, "re-owning a build secret", "secret", key)
		}
	}
}

// stillPublished reports whether what this object last published is still in the registry.
//
// The composer has always asked this, with one HEAD, because it can rebuild identical bytes if the
// answer is no. This kind could not: a rebuild produces a DIFFERENT digest, so for a release the
// question was not asked at all and a lost image simply stayed lost while the object reported
// Ready. That is now decided the other way -- see ADR 0051 for what it costs.
//
// Only a definite 404 counts as missing. Every other outcome -- unreachable registry, expired
// credential, timeout -- answers true, because the alternative is that one registry outage starts a
// build for every ImageBuild in the cluster at once. Fail towards doing nothing.
//
// The tags come from the SPEC, never from status.artifact.tags: those are stored through the public
// host, which is exactly the trap ADR 0048 was written about.
func (r *ImageBuildReconciler) stillPublished(ctx context.Context, obj *ociv1alpha1.ImageBuild) bool {
	prev := obj.Status.Artifact
	if prev == nil || prev.Digest == "" {
		return true
	}
	reg, err := r.registryFor(ctx, obj)
	if err != nil || reg.repo == "" {
		return true
	}
	repo := reg.repo

	// The digest, then every tag the spec asks for. A tag is checked even though the content it
	// names may still exist under its digest: an untagged manifest is precisely what the shipped
	// deleteUntagged policy reclaims next, so a lost tag is a loss in progress rather than a
	// cosmetic one. It is also the shape the field report took -- tags gone, manifest alive.
	refs := []string{repo + "@" + prev.Digest}
	if p := obj.Spec.Push; p != nil {
		if tags, err := recon.EffectiveTags(p.GetTags(), p.GetRef()); err == nil {
			for _, t := range tags {
				refs = append(refs, repo+":"+t)
			}
		}
	}

	for _, ref := range refs {
		parsed, err := name.ParseReference(ref, reg.refOpts...)
		if err != nil {
			// Unparseable is not evidence of absence, and this fails towards doing nothing.
			continue
		}
		if _, err := remote.Head(parsed, reg.opts...); recon.IsNotFound(err) {
			return false
		}
	}
	return true
}

// registryAccess is everything needed to ask this object's registry a question.
type registryAccess struct {
	// repo is empty when the object names no repository and no default registry is configured, so
	// there is nowhere to ask about.
	repo    string
	refOpts []name.Option
	opts    []remote.Option
}

// registryFor resolves that once, for every caller.
//
// The tag-conflict check and the published-artifact check ask the SAME registry about the SAME
// object, so any difference between how they reach it could only be a bug. The insecure-host half
// is the one that would bite: a mismatch there surfaces as a TLS error that looks nothing like the
// missing --insecure-registry entry causing it.
func (r *ImageBuildReconciler) registryFor(
	ctx context.Context, obj *ociv1alpha1.ImageBuild,
) (registryAccess, error) {
	repo := r.repositoryFor(obj)
	if repo == "" {
		return registryAccess{}, nil
	}
	opts, err := r.remoteOptions(ctx, obj)
	if err != nil {
		return registryAccess{}, err
	}
	access := registryAccess{repo: repo, opts: opts}
	if recon.InsecureHost(repo, r.JobConfig.InsecureRegistries) {
		access.refOpts = append(access.refOpts, name.Insecure)
	}
	return access, nil
}
