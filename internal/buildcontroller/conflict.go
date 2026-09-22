package buildcontroller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

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

// checkTagConflict is a pre-flight for spec.push.onConflict, run before a Job is created so a
// build is not spent on a tag that already holds foreign content. applyTags enforces the policy
// exactly after the build. Returns whether the caller should stop, and the divergence to record.
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
		// Nothing to ask the registry, so Overwrite also works when registry reads fail.
		return false, nil, nil
	}

	tags, err := recon.EffectiveTags(p.GetTags(), p.GetRef())
	if err != nil {
		return false, nil, err
	}
	if len(tags) == 0 {
		// Digest-only publication cannot collide.
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

	// Approximate: the output digest is unknown yet, so status's digest stands in for it. That
	// exempts a tag holding this object's own previous digest; applyTags closes that (ADR 0054).
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
			// Nothing was built, so Dropped stays empty.
			ObservedAt: &now,
		}, nil
	default:
		// Unreachable (CEL enforces the enum), but refuse rather than silently overwrite if a new
		// value is added without a branch here.
		return false, nil, recon.Terminal("unknown onConflict policy %q", policy)
	}
}

// remoteOptions builds registry auth from spec.push.secretRef, the same Secret the Job is given.
func (r *ImageBuildReconciler) remoteOptions(
	ctx context.Context, obj *ociv1alpha1.ImageBuild,
) ([]remote.Option, error) {
	return recon.RemoteAuth{
		Reader:    r.Client,
		Transport: r.Transport,
		Default:   r.Default,
	}.Options(ctx, obj.Namespace, r.repositoryFor(obj), obj.Spec.Push)
}

// cacheAvailable reports whether this object's build cache reference resolves, since BuildKit
// fails the build on an unresolvable cache import (see buildctlArgs). Any error answers "no": the
// worst case is repopulating an existing cache.
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

// repositoryFor is the one place that resolves where a build publishes; the push, conflict check,
// cache and retention must all agree.
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
// creating a per-build copy of the operator's when the object has none of its own (a pod mounts
// Secrets only from its own namespace).
//
// The copy is named after the Job and dies with it, which bounds, but does not remove, the
// exposure: while a build runs, anyone who can read Secrets in that namespace can read the
// operator's credential. Tolerable because the namespace can already push there via an ImageBuild.
func (r *ImageBuildReconciler) pushSecretFor(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, jobName string,
) (string, error) {
	if p := obj.Spec.Push; p != nil && p.SecretRef != nil {
		return p.SecretRef.Name, nil
	}

	repo := r.repositoryFor(obj)
	if r.Default.SecretName == "" || !r.Default.Owns(repo) {
		// No operator credential, or not the operator's registry: push anonymously.
		return "", nil
	}

	var operatorSecret corev1.Secret
	key := types.NamespacedName{Namespace: r.Default.Namespace, Name: r.Default.SecretName}
	if err := r.Get(ctx, key, &operatorSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", recon.Pending("default push secret %s not found yet", key)
		}
		return "", fmt.Errorf("reading default push secret %s: %w", key, err)
	}

	copied := perBuildSecret(obj, jobName+"-push", "Short-lived copy of the operator's registry credential, "+
		"mounted by this build's Job. Owned by this build's Job and deleted with it.")
	copied.Type = operatorSecret.Type
	copied.Data = operatorSecret.Data
	// On a retry the copy is refreshed, so a rotated password applies.
	if err := r.createBuildSecret(ctx, obj, copied, "push credential"); err != nil {
		return "", err
	}
	return copied.Name, nil
}

// registryCASecretFor copies the operator's registry CA into the build's namespace, like
// pushSecretFor. A separate Secret rather than a key on the push copy, because a build with its
// own spec.push.secretRef gets no push copy but still needs the CA. A Secret rather than a
// ConfigMap only because the builder already has write access to Secrets.
func (r *ImageBuildReconciler) registryCASecretFor(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, jobName string,
) (string, error) {
	if len(r.JobConfig.RegistryCA) == 0 {
		return "", nil
	}

	ca := perBuildSecret(obj, jobName+"-registry-ca", "The registry CA this build's Job trusts. "+
		"Not secret; a Secret only because the builder already has permission to "+
		"write Secrets here. Owned by this build's Job and deleted with it.")
	ca.Data = map[string][]byte{"ca.crt": r.JobConfig.RegistryCA}
	// On a retry the copy is refreshed, so a rotated CA applies.
	if err := r.createBuildSecret(ctx, obj, ca, "registry CA"); err != nil {
		return "", err
	}
	return ca.Name, nil
}

// dockerfileSecretFor copies a Dockerfile that is not in the build context into a Secret the Job
// mounts.
//
// It holds the exact bytes CheckPinnedBases approved. Projecting the source object directly would
// let an edit between the check and pod start build an unchecked Dockerfile. Immutable, with no
// update path: the content is in the input hash, and so in the name. A Secret only because the
// builder already has write access to Secrets.
func (r *ImageBuildReconciler) dockerfileSecretFor(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, jobName string, content []byte,
) (string, error) {
	if len(content) == 0 {
		return "", nil
	}

	df := perBuildSecret(obj, jobName+"-dockerfile", "The Dockerfile this build runs, as checked by the "+
		"controller. Not secret; a Secret only because the builder already has "+
		"permission to write Secrets here. Owned by this build's Job and deleted with it.")
	df.Immutable = ptr.To(true)
	df.Data = map[string][]byte{dockerfileKey: content}
	if err := r.createBuildSecret(ctx, obj, df, "Dockerfile"); err != nil {
		return "", err
	}
	return df.Name, nil
}

// contextTokenFor mints the per-build bearer token the build pod uses to fetch its own context.
// The Secret is named for the Job, so a token opens only the build it was minted for. Immutable
// and never overwritten, so a retry reuses whatever token the endpoint accepts.
func (r *ImageBuildReconciler) contextTokenFor(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, jobName string,
) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a context token: %w", err)
	}

	secret := perBuildSecret(obj, contextSecretName(jobName), "Lets this build fetch its own source through the "+
		"builder, so the build pod never reaches source-controller. Owned by this build's Job and deleted with it.")
	secret.Immutable = ptr.To(true)
	secret.Data = map[string][]byte{contextTokenKey: []byte(hex.EncodeToString(raw))}
	if err := r.createBuildSecret(ctx, obj, secret, "context token"); err != nil {
		return "", err
	}
	return secret.Name, nil
}

// perBuildSecret is the skeleton of every Secret this controller creates for one build: in the
// object's namespace, labelled as the builder's, and described for whoever finds it.
func perBuildSecret(obj *ociv1alpha1.ImageBuild, name, description string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   obj.Namespace,
			Labels:      map[string]string{ManagedByLabel: builderManager},
			Annotations: map[string]string{"oci.lhns.de/description": description},
		},
		Type: corev1.SecretTypeOpaque,
	}
}

// createBuildSecret creates sec owned by obj; adoptBuildSecrets later re-owns it by the Job. If it
// already exists, a mutable Secret has its data refreshed and an immutable one (whose name carries
// its content) is left alone. what names the Secret in errors.
func (r *ImageBuildReconciler) createBuildSecret(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, sec *corev1.Secret, what string,
) error {
	if err := ctrl.SetControllerReference(obj, sec, r.Scheme()); err != nil {
		return fmt.Errorf("setting owner on the %s: %w", what, err)
	}
	err := r.Create(ctx, sec)
	switch {
	case err == nil:
		return nil
	case !apierrors.IsAlreadyExists(err):
		return fmt.Errorf("creating the %s: %w", what, err)
	case ptr.Deref(sec.Immutable, false):
		return nil
	}
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sec), existing); err != nil {
		return fmt.Errorf("reading the existing %s: %w", what, err)
	}
	existing.Data = sec.Data
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("refreshing the %s: %w", what, err)
	}
	return nil
}

// buildSecretNames are the Secrets this controller creates for one Job. Derived, not collected
// from what pushSecretFor returned, which may be the user's own Secret.
func buildSecretNames(jobName string) []string {
	return []string{
		jobName + "-push",
		jobName + "-registry-ca",
		jobName + "-dockerfile",
		contextSecretName(jobName),
	}
}

// adoptBuildSecrets re-owns each per-build Secret by the Job that mounts it.
//
// They are created owned by the ImageBuild because the Job does not exist yet; left that way they
// would accumulate for the object's whole life. The Job's TTL and retry deletion then reclaim
// them, with no list-and-prune sweep (which would need `list` on Secrets). ADR 0050.
func (r *ImageBuildReconciler) adoptBuildSecrets(ctx context.Context, obj *ociv1alpha1.ImageBuild, job *batchv1.Job) {
	log := logf.FromContext(ctx)
	for _, name := range buildSecretNames(job.Name) {
		var sec corev1.Secret
		key := types.NamespacedName{Namespace: job.Namespace, Name: name}
		if err := r.Get(ctx, key, &sec); err != nil {
			// Most builds lack most of these Secrets.
			if !apierrors.IsNotFound(err) {
				log.Error(err, "reading a build secret to re-own it", "secret", key)
			}
			continue
		}
		// Only ever touch a Secret this controller created: generated name, own label, and
		// currently controlled by this object.
		if sec.Labels[ManagedByLabel] != builderManager {
			continue
		}
		if !metav1.IsControlledBy(&sec, obj) {
			continue
		}
		// Never fatal: the Job is running, and the worst case is a leak.
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

// stillPublished reports whether what this object last published is still in the registry. A
// rebuild yields a different digest (ADR 0051).
//
// Only a definite 404 counts as missing; any other error answers true, so a registry outage does
// not rebuild every ImageBuild at once. Tags come from the spec, never from status.artifact.tags,
// which hold the public host (ADR 0048).
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

	// The digest, then every spec tag: an untagged manifest is what deleteUntagged reclaims next,
	// so a lost tag is a loss in progress.
	refs := []string{repo + "@" + prev.Digest}
	if p := obj.Spec.Push; p != nil {
		if tags, err := recon.EffectiveTags(p.GetTags(), p.GetRef()); err == nil {
			for _, t := range tags {
				refs = append(refs, repo+":"+t)
			}
		}
	}
	// The digest's own tag only once status records it, or an upgrade would rebuild every object.
	// backfillDigestTags adds it.
	if recon.HasDigestTag(prev.Tags, prev.Digest) {
		refs = append(refs, repo+":"+recon.DigestTag(prev.Digest))
	}

	for _, ref := range refs {
		parsed, err := name.ParseReference(ref, reg.refOpts...)
		if err != nil {
			// Unparseable is not evidence of absence.
			continue
		}
		if _, err := remote.Head(parsed, reg.opts...); recon.IsNotFound(err) {
			return false
		}
	}
	return true
}

// backfillDigestTags gives content published before ADR 0060 its digest's own tag: the current
// artifact and every history entry (which a rollback pulls). Driven by status, so it costs nothing
// once converged, and it needs only where the object publishes, so Reconcile runs it for suspended
// objects and ones whose spec is otherwise invalid. Referrers need nothing here: BuildKit's
// attestations are children of the image index, and a signature is a tag. Best effort: failures are
// logged and retried next pass.
func (r *ImageBuildReconciler) backfillDigestTags(ctx context.Context, obj *ociv1alpha1.ImageBuild) {
	art := obj.Status.Artifact
	pending := art != nil && art.Digest != "" && !recon.HasDigestTag(art.Tags, art.Digest)
	for _, rec := range obj.Status.History {
		if rec.Digest != "" && !recon.HasDigestTag(rec.Tags, rec.Digest) {
			pending = true
		}
	}
	if !pending {
		return
	}

	reg, err := r.registryFor(ctx, obj)
	if err != nil || reg.repo == "" {
		return
	}
	public := r.Default.PublicRepository(reg.repo)
	log := logf.FromContext(ctx)

	// Applied once per digest even when the artifact and a history entry share it.
	applied := map[string]bool{}
	apply := func(digest string) bool {
		if done, seen := applied[digest]; seen {
			return done
		}
		err := recon.ApplyDigestTag(reg.repo, digest, reg.refOpts, reg.opts)
		if err != nil && !recon.IsNotFound(err) {
			log.Error(err, "applying the digest's own tag; will retry", "digest", digest)
		}
		applied[digest] = err == nil
		return err == nil
	}
	own := func(digest string) string { return public + ":" + recon.DigestTag(digest) }

	if art != nil && art.Digest != "" && !recon.HasDigestTag(art.Tags, art.Digest) && apply(art.Digest) {
		art.Tags = append(art.Tags, own(art.Digest))
	}
	for i := range obj.Status.History {
		rec := &obj.Status.History[i]
		if rec.Digest != "" && !recon.HasDigestTag(rec.Tags, rec.Digest) && apply(rec.Digest) {
			rec.Tags = append(rec.Tags, own(rec.Digest))
		}
	}
}

// registryAccess is everything needed to ask this object's registry a question.
type registryAccess struct {
	// repo is empty when there is no repository and no default registry.
	repo    string
	refOpts []name.Option
	opts    []remote.Option
}

// registryFor resolves how to reach this object's registry, shared by every caller so they cannot
// disagree (an insecure-host mismatch would surface as a baffling TLS error).
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

// applyTags names what the build pushed, and is where onConflict is enforced exactly, with the new
// digest in hand (ADR 0054).
//
// Returns a conflict record when onConflict: Keep left the tag alone. Under Fail it refuses
// terminally having tagged nothing, not even the digest's own tag, so deleteUntagged reclaims the
// manifest. Every accepted publish also gets its digest's own tag (ADR 0060).
func (r *ImageBuildReconciler) applyTags(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, digest string,
) (*ociv1alpha1.TagConflictStatus, error) {
	p := obj.Spec.Push
	tags, err := recon.EffectiveTags(p.GetTags(), p.GetRef())
	if err != nil {
		return nil, err
	}

	reg, err := r.registryFor(ctx, obj)
	if err != nil {
		return nil, err
	}
	if reg.repo == "" {
		return nil, recon.Pending(
			"this build names no push.repository, and no default registry is configured")
	}

	published, err := recon.ResolvePublished(reg.repo, tags, obj.Status.Artifact, reg.refOpts, reg.opts)
	if err != nil {
		return nil, err
	}

	if tag, current := published.Conflicts(tags, digest); tag != "" {
		switch p.ResolveConflictPolicy() {
		case ociv1alpha1.ConflictFail:
			return nil, recon.Terminal(
				"tag %s already resolves to %s and this build produced %s; change the tag, or set "+
					"onConflict: Overwrite if it is meant to move, or onConflict: Keep to leave it "+
					"alone", tag, current, digest)
		case ociv1alpha1.ConflictKeep:
			now := metav1.Now()
			// The real dropped digest, available because the Job pushes before naming.
			return &ociv1alpha1.TagConflictStatus{
				Tag: tag, Existing: current, Dropped: digest, ObservedAt: &now,
			}, nil
		}
	}

	desc, err := remote.Get(mustDigestRef(reg.repo, digest, reg.refOpts), reg.opts...)
	switch {
	case recon.IsNotFound(err):
		// Pending, not terminal: no spec edit fixes it. The manifest is untagged until named here,
		// so the registry may be lagging or its collector may have reclaimed it (NAME_UNKNOWN if
		// the repository went with it). The message names both.
		return nil, recon.Pending(
			"the build produced %s but %s does not serve it; the manifest is untagged until this "+
				"controller names it, so either the registry has not caught up or its collector "+
				"reclaimed it first -- check the registry's gcDelay if this persists",
			digest, reg.repo)
	case err != nil:
		return nil, fmt.Errorf("reading the pushed manifest %s: %w", digest, err)
	}
	for _, tag := range recon.PublishTags(tags, digest) {
		ref, err := name.NewTag(reg.repo+":"+tag, reg.refOpts...)
		if err != nil {
			return nil, recon.Terminal("invalid tag %q: %v", tag, err)
		}
		if err := remote.Tag(ref, desc, reg.opts...); err != nil {
			// Not terminal: the content is pushed and a retry re-applies the same names.
			return nil, fmt.Errorf("tagging %s as %s: %w", digest, tag, err)
		}
	}
	return nil, nil
}

// mustDigestRef builds the by-digest reference for content this controller just pushed. The
// digest comes from buildctl, so a parse failure is not a user error; the zero value makes the Get
// fail with a message naming the reference.
func mustDigestRef(repo, digest string, opts []name.Option) name.Reference {
	ref, err := name.NewDigest(repo+"@"+digest, opts...)
	if err != nil {
		return name.Digest{}
	}
	return ref
}
