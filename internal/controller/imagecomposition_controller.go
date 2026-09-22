package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/attest"
	"github.com/lhns/kube-oci-composer/internal/cache"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
	"github.com/lhns/kube-oci-composer/internal/retention"
)

// pendingRetryInterval is how often a composition waiting on a dependency re-checks: short enough
// that a same-commit apply converges unnoticed, long enough not to be a hot loop.
const pendingRetryInterval = 30 * time.Second

// ImageCompositionReconciler assembles and publishes OCI artifacts.
type ImageCompositionReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Refresher renews the lease on an artifact the moment it is published. Nil disables it
	// (--retention-refresh-interval=0).
	Refresher *retention.Refresher

	// Default is where objects publish when they name no repository of their own.
	Default recon.DefaultRegistry

	// Attestor attaches the SBOM, provenance and signature, when any of them is enabled. Nil or
	// disabled changes nothing about the reconcile.
	Attestor *attest.Attestor

	// Export is what the operator decides about push.writeRefTo. The controller, not RBAC, is the
	// boundary here (ADR 0056).
	Export recon.ExportOptions

	// Transport, when set, trusts an additional CA on top of the system roots, for EVERY registry
	// this controller talks to (base images may come from the bundled registry too).
	Transport http.RoundTripper

	// InsecureRegistries are hosts this controller may push to over plain HTTP (recon.InsecureHost).
	// The default bundled registry has no certificate (ADR 0035).
	InsecureRegistries []string

	// RequirePinnedSources refuses any sourceRef that names no revision (threat T1). Off by
	// default, since pinning is optional per ADR 0026.
	RequirePinnedSources bool

	// Fetcher retrieves layer content from its origin.
	Fetcher *oci.Fetcher

	// Cache resolves layer digests to local files, falling back to Fetcher on a miss. Optional;
	// without it every build fetches from the origin.
	Cache *cache.Cache

	// Readiness gates the pod's readiness probe until the served store is warm. Optional; when
	// nil, readiness is not tracked.
	Readiness *Readiness

	// HistoryLimit is how many past builds to retain per object when the object does not say.
	// Zero means DefaultHistoryLimit.
	HistoryLimit int
}

// The controller only observes ImageCompositions and patches their finalizers and status; the
// verbs grant no more than that.
//
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagecompositions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagecompositions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagecompositions/finalizers,verbs=update
// Secrets are read by name and NOT cached (see cmd/oci-composer), so `get` alone suffices;
// caching would need list/watch on every Secret in the cluster.
//
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
//
// ConfigMaps ARE cached and watched, so a configMapRef edit rebuilds promptly; create, update and
// delete are for push.writeRefTo exports.
//
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete
//
// Flux sources are read-only, for their status.artifact.
//
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories;ocirepositories;buckets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch

func (r *ImageCompositionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var obj ociv1alpha1.ImageComposition
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		if apierrors.IsNotFound(err) {
			if r.Readiness != nil {
				r.Readiness.Forget(req.NamespacedName)
			}
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// On every branch: readiness means "reconciled", not "healthy". See Readiness.Observe.
	if r.Readiness != nil {
		defer r.Readiness.Observe(req.NamespacedName)
	}

	if !obj.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &obj)
	}

	if !controllerutil.ContainsFinalizer(&obj, ociv1alpha1.Finalizer) {
		patch := client.MergeFrom(obj.DeepCopy())
		controllerutil.AddFinalizer(&obj, ociv1alpha1.Finalizer)
		if err := r.Patch(ctx, &obj, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	// Suspend halts work without touching what is already published.
	if obj.Spec.Suspend {
		return ctrl.Result{}, r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
			recon.SetCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
				ociv1alpha1.ReasonSuspended, "Reconciliation is suspended")
			recon.RemoveCondition(o, ociv1alpha1.ReconcilingCondition)
			recon.RemoveCondition(o, ociv1alpha1.StalledCondition)
		})
	}

	interval := recon.Interval(obj.Spec.Interval)

	result, err := r.reconcileArtifact(ctx, &obj)
	if err != nil {
		// A missing dependency is a normal step in converging: a quiet fixed-interval retry, and
		// never Stalled, since the object that fixes it raises no event here.
		if recon.IsPending(err) {
			logger.Info("waiting on a dependency; will retry", "reason", err.Error(),
				"retryIn", pendingRetryInterval)
			return ctrl.Result{RequeueAfter: pendingRetryInterval},
				r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
					recon.SetCondition(o, ociv1alpha1.ReconcilingCondition, metav1.ConditionTrue,
						ociv1alpha1.ReasonDependencyNotReady, err.Error())
					recon.SetCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
						ociv1alpha1.ReasonDependencyNotReady, err.Error())
					recon.RemoveCondition(o, ociv1alpha1.StalledCondition)
				})
		}

		if recon.IsTerminal(err) {
			logger.Error(err, "terminal error; not retrying until the spec changes")
			recon.Event(r.Recorder, &obj, corev1.EventTypeWarning, reasonFor(err), err.Error())
			// No requeue: the generation change that fixes it wakes the controller.
			return ctrl.Result{}, r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
				recon.SetCondition(o, ociv1alpha1.StalledCondition, metav1.ConditionTrue, reasonFor(err), err.Error())
				recon.SetCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse, reasonFor(err), err.Error())
				recon.RemoveCondition(o, ociv1alpha1.ReconcilingCondition)
			})
		}

		logger.Error(err, "transient failure; will retry")
		recon.Event(r.Recorder, &obj, corev1.EventTypeWarning, ociv1alpha1.ReasonFetchFailed, err.Error())
		if perr := r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
			recon.SetCondition(o, ociv1alpha1.ReconcilingCondition, metav1.ConditionTrue,
				ociv1alpha1.ReasonProgressing, err.Error())
			recon.SetCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
				ociv1alpha1.ReasonProgressing, err.Error())
			recon.RemoveCondition(o, ociv1alpha1.StalledCondition)
		}); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, err // exponential backoff
	}

	if err := r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
		o.Status.Artifact = result.Artifact
		o.Status.Attestations = result.Attestations
		o.Status.InputHash = result.InputHash
		markDigestTagged(o.Status.History, result.DigestTagged)
		o.Status.History = recon.RecordHistory(o.Status.History, result.Record, r.historyLimit(o))
		// Assigned even when nil, so a resolved divergence stops being reported.
		o.Status.Conflict = result.Conflict
		msg := fmt.Sprintf("Published %s", result.Artifact.Ref)
		if c := result.Conflict; c != nil {
			// Ready, but the message says what was kept and what was dropped.
			msg = fmt.Sprintf("Kept %s at %s; dropped %s (onConflict: Keep)",
				c.Tag, c.Existing, c.Dropped)
		}
		recon.SetCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonSucceeded, msg)
		recon.RemoveCondition(o, ociv1alpha1.ReconcilingCondition)
		recon.RemoveCondition(o, ociv1alpha1.StalledCondition)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// A nil Record means nothing was published, so nothing is newly unprotected.
	if result.Record != nil {
		r.refreshNow(ctx, &obj, result.Artifact)
	}

	// After the status write, so what is exported is exactly what status reports.
	if err := r.exportRef(ctx, &obj, result.Artifact); err != nil {
		if recon.IsTerminal(err) {
			recon.Event(r.Recorder, &obj, corev1.EventTypeWarning, ociv1alpha1.ReasonInvalidSpec, err.Error())
			return ctrl.Result{}, r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
				recon.SetCondition(o, ociv1alpha1.StalledCondition, metav1.ConditionTrue,
					ociv1alpha1.ReasonInvalidSpec, err.Error())
				recon.SetCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
					ociv1alpha1.ReasonInvalidSpec, err.Error())
			})
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

// buildResult is what one reconcile produced.
type buildResult struct {
	// Artifact is the published reference. Always set on success.
	Artifact *ociv1alpha1.ArtifactStatus
	// InputHash is the hash of everything that determined the output.
	InputHash string
	// Attestations records what supply-chain material is attached, so the next reconcile can tell
	// there is nothing to do without asking the registry.
	Attestations *ociv1alpha1.AttestationStatus
	// Conflict is set when onConflict: Keep left an existing tag in place and dropped what this
	// reconcile produced. Copied into status so the divergence is visible rather than inferred.
	Conflict *ociv1alpha1.TagConflictStatus
	// Record describes a NEW build, and is nil when the reconcile converged without publishing,
	// so history is not padded with duplicates.
	Record *ociv1alpha1.BuildRecord
	// DigestTagged are history digests given their own tag on this pass (ADR 0060's backfill).
	// Reported rather than written, because status is patched against a fresh read.
	DigestTagged []string
}

// buildRecord captures the blobs a build is composed of, so garbage collection can tell what is
// still live. For an index, Blobs is the union of every child's config and layers, and Manifests
// names the children, so per-platform configs are not reclaimed under a live index.
func buildRecord(art builtArtifact, tags []string, digest v1.Hash, inputs []oci.LayerInput) (*ociv1alpha1.BuildRecord, error) {
	children, err := art.children()
	if err != nil {
		return nil, err
	}

	blobs := make([]string, 0, len(children)*2)
	seen := make(map[string]struct{})
	add := func(d string) {
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		blobs = append(blobs, d)
	}

	for _, img := range children {
		cfg, err := img.ConfigName()
		if err != nil {
			return nil, fmt.Errorf("config digest: %w", err)
		}
		add(cfg.String())
		layers, err := img.Layers()
		if err != nil {
			return nil, fmt.Errorf("layers: %w", err)
		}
		for _, l := range layers {
			d, err := l.Digest()
			if err != nil {
				return nil, fmt.Errorf("layer digest: %w", err)
			}
			add(d.String())
		}
	}

	manifests, err := art.childDigests()
	if err != nil {
		return nil, err
	}

	now := metav1.Now()
	return &ociv1alpha1.BuildRecord{
		Tags:      append([]string(nil), tags...),
		Digest:    digest.String(),
		Blobs:     blobs,
		Manifests: manifests,
		Sources:   sourceRecords(inputs),
		Time:      &now,
	}, nil
}

// sourceRecords is where each layer's content came from. Layers without a revision still record
// their digest, which for a fetch is the identity.
func sourceRecords(inputs []oci.LayerInput) []ociv1alpha1.SourceRecord {
	if len(inputs) == 0 {
		return nil
	}
	out := make([]ociv1alpha1.SourceRecord, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, ociv1alpha1.SourceRecord{
			Name:     in.Name,
			Revision: in.Identity,
			Digest:   in.Digest,
		})
	}
	return out
}

// tagSuffix renders tags for an event message, and nothing at all when there are none.
func tagSuffix(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return " as " + strings.Join(tags, ", ")
}

// reconcileArtifact does the work and returns what is published.
//
// Ordered by cost: the input hash comes from the spec alone, so the common "nothing changed" case
// costs a few HEADs. Only past that is anything fetched, and only past the digest comparison is
// anything written.
func (r *ImageCompositionReconciler) reconcileArtifact(ctx context.Context, obj *ociv1alpha1.ImageComposition) (buildResult, error) {
	// Created before resolution: it also holds content synthesised while resolving (ConfigMaps).
	workDir, err := os.MkdirTemp("", "oci-composer-work-*")
	if err != nil {
		return buildResult{}, fmt.Errorf("creating work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	inputs, imagePulls, err := r.resolveInputs(ctx, obj, workDir)
	if err != nil {
		return buildResult{}, err
	}

	cfg := configFrom(obj.Spec.Config)

	// Platforms knowable WITHOUT fetching: the declared list, else the base's (pinned by the base
	// digest), else, with no base, the controller's own.
	declared, err := declaredPlatforms(obj)
	if err != nil {
		return buildResult{}, err
	}
	hashPlatforms := declared
	var baseDigest string
	if obj.Spec.Base != nil {
		// From the accessor, not the field, so `ref` and `image`+`digest` spellings hash the same.
		_, baseDigest = obj.Spec.Base.Repository()
	} else if len(hashPlatforms) == 0 {
		hashPlatforms = []oci.Platform{oci.RuntimePlatform()}
	}
	inputHash := oci.InputHash(inputs, cfg, baseDigest, hashPlatforms)

	tgt, err := r.target(obj)
	if err != nil {
		return buildResult{}, err
	}

	opts, err := r.remoteOptions(ctx, obj, tgt.writeRepo)
	if err != nil {
		return buildResult{}, err
	}

	refOpts := r.refOptions(tgt.writeRepo)

	// The digest's own tag is checked only once status claims it; otherwise an object published
	// before ADR 0060 would miss the cheap path and be reassembled just to add a name, which
	// backfillDigestTags does for one request.
	resolve := tgt.tags
	if prev := obj.Status.Artifact; prev != nil && recon.HasDigestTag(prev.Tags, prev.Digest) {
		resolve = recon.PublishTags(tgt.tags, prev.Digest)
	}
	published, err := recon.ResolvePublished(tgt.writeRepo, resolve, obj.Status.Artifact, refOpts, opts)
	if err != nil {
		return buildResult{}, err
	}

	// The cheap path: same inputs, and everything they produced is still published under every
	// name, so there is nothing to do. This is what makes interval reconciles nearly free.
	if prev := obj.Status.Artifact; prev != nil &&
		obj.Status.InputHash == inputHash &&
		published.Matches(prev.Digest) &&
		// Reads only status, so a converged reconcile costs no extra registry requests.
		r.Attestor.Complete(attestRecord(obj.Status.Attestations), prev.Digest) {
		art := prev.DeepCopy()
		tagged := r.backfillDigestTags(ctx, obj, tgt, art, refOpts, opts)
		return buildResult{Artifact: art, InputHash: inputHash,
			Attestations: obj.Status.Attestations.DeepCopy(), DigestTagged: tagged}, nil
	}

	for i := range inputs {
		// Already on disk (a ConfigMap), or a remove entry with nothing to fetch.
		if inputs[i].Path != "" || len(inputs[i].Remove) > 0 {
			continue
		}

		// An image layer is a manifest, not a blob, so it skips the layer cache.
		if src, ok := imagePulls[i]; ok {
			img, err := r.pullImageLayer(ctx, obj, inputs[i], src)
			if err != nil {
				return buildResult{}, err
			}
			inputs[i].Image = img
			continue
		}

		path, err := r.resolveLayer(ctx, inputs[i])
		if err != nil {
			var dm *oci.ErrDigestMismatch
			if errors.As(err, &dm) {
				// Terminal: the declared digest and the served bytes disagree.
				return buildResult{}, recon.Terminal("layer %q: %s", inputs[i].Name, dm.Error())
			}
			return buildResult{}, fmt.Errorf("layer %q: %w", inputs[i].Name, err)
		}
		inputs[i].Path = path
	}

	art, err := r.assemble(ctx, obj, declared, inputs, cfg, workDir)
	if err != nil {
		var unsupported *oci.ErrUnsupportedUnpack
		if errors.As(err, &unsupported) {
			// Terminal: retrying cannot add a code path to this binary.
			return buildResult{}, recon.Terminal("%s", unsupported.Error())
		}
		return buildResult{}, err
	}

	digest, err := art.Digest()
	if err != nil {
		return buildResult{}, fmt.Errorf("computing digest: %w", err)
	}

	// Second convergence check, against the real output digest, for a changed input hash with
	// unchanged output. It must run BEFORE the conflict guard, or republishing identical content
	// under an immutable tag would fail.
	if published.Matches(digest.String()) {
		// The digest's own tag may be missing (it was only checked if status claimed it).
		if err := recon.ApplyDigestTag(tgt.writeRepo, digest.String(), refOpts, opts); err != nil {
			return buildResult{}, err
		}
		// Attested here too, so newly enabling attestation takes effect without a content change.
		return buildResult{
			Artifact:     artifactStatus(tgt, digest, true),
			InputHash:    inputHash,
			Attestations: r.attestPublished(ctx, obj, tgt, digest, inputs, baseDigest, refOpts, opts),
		}, nil
	}

	// Checked before anything is written, so a partial rename cannot happen.
	if tag, cur := published.Conflicts(tgt.tags, digest.String()); tag != "" {
		switch tgt.onConflict {
		case ociv1alpha1.ConflictFail:
			// Refuse to change what a tag means.
			return buildResult{}, recon.Terminal(
				"tag %s already resolves to %s but this spec produces %s; change the tag, or set "+
					"onConflict: Overwrite if it is meant to move", tag, cur, digest)
		case ociv1alpha1.ConflictKeep:
			// Publish nothing. status.artifact reports the EXISTING digest, since that is what a
			// consumer pulls; the dropped digest is recorded in the conflict so the divergence is
			// visible (ADR 0026).
			existing, err := v1.NewHash(cur)
			if err != nil {
				return buildResult{}, recon.Terminal(
					"tag %s resolves to %q, which is not a digest: %v", tag, cur, err)
			}
			now := metav1.Now()
			return buildResult{
				// Without the digest's own tag: nothing was written. The backfill adds it later.
				Artifact:  artifactStatus(tgt, existing, false),
				InputHash: inputHash,
				Conflict: &ociv1alpha1.TagConflictStatus{
					Tag:        tag,
					Existing:   cur,
					Dropped:    digest.String(),
					ObservedAt: &now,
				},
			}, nil
		}
	}

	// The digest reference first, so a failure part-way leaves the content addressable.
	digestRef, err := name.ParseReference(fmt.Sprintf("%s@%s", tgt.writeRepo, digest), refOpts...)
	if err != nil {
		return buildResult{}, recon.Terminal("invalid reference %s@%s: %v", tgt.writeRepo, digest, err)
	}
	if err := art.write(digestRef, opts...); err != nil {
		return buildResult{}, fmt.Errorf("publishing %s: %w", digestRef, err)
	}

	// The digest's own tag with the rest, so a later tag move cannot take this content with it
	// (ADR 0060).
	for _, tag := range recon.PublishTags(tgt.tags, digest.String()) {
		ref, err := name.ParseReference(fmt.Sprintf("%s:%s", tgt.writeRepo, tag), refOpts...)
		if err != nil {
			return buildResult{}, recon.Terminal("invalid reference %s:%s: %v", tgt.writeRepo, tag, err)
		}
		if err := art.write(ref, opts...); err != nil {
			return buildResult{}, fmt.Errorf("publishing %s: %w", ref, err)
		}
	}

	// After publishing, so a signature never describes something unpublished.
	attestations := r.attestPublished(ctx, obj, tgt, digest, inputs, baseDigest, refOpts, opts)

	recon.Event(r.Recorder, obj, corev1.EventTypeNormal, ociv1alpha1.ReasonSucceeded,
		fmt.Sprintf("Published %s@%s%s", tgt.pullRepo, digest, tagSuffix(tgt.tags)))

	record, err := buildRecord(art, recon.PublishTags(tgt.tags, digest.String()), digest, inputs)
	if err != nil {
		// The artifact is published; only its retention record could not be built.
		return buildResult{}, fmt.Errorf("recording build %s: %w", digest, err)
	}

	return buildResult{
		Artifact:     artifactStatus(tgt, digest, true),
		InputHash:    inputHash,
		Record:       record,
		Attestations: attestations,
	}, nil
}

// historyLimit resolves the retention count for one object.
func (r *ImageCompositionReconciler) historyLimit(obj *ociv1alpha1.ImageComposition) int {
	return obj.Spec.Push.HistoryLimit(r.HistoryLimit)
}

// resolveLayer returns a local path holding the layer's content.
//
// With a cache, the returned path is owned by the cache and must NOT be removed by the caller.
func (r *ImageCompositionReconciler) resolveLayer(ctx context.Context, in oci.LayerInput) (string, error) {
	fetch := func(ctx context.Context, digest string) (string, error) {
		return r.Fetcher.FetchURL(ctx, in.URL, digest)
	}
	if r.Cache == nil {
		return fetch(ctx, in.Digest)
	}
	return r.Cache.Path(ctx, in.Digest, fetch)
}

// target is where an artifact is written and how it should be referenced.
//
// writeRepo and pullRepo differ when the controller (cluster DNS) and the kubelet (node resolver)
// need different names for one registry; see recon.DefaultRegistry.PublicHost.
type target struct {
	// writeRepo is what the controller pushes to, checks tags against, and refreshes. Everything
	// that opens a connection uses this one.
	writeRepo string
	// pullRepo is what a workload references, and it appears in status.artifact.ref and nowhere
	// else. The controller never connects to it and may well be unable to resolve it.
	pullRepo string
	// tags are the tags to publish under, in order. Empty means publish by digest alone.
	tags []string
	// onConflict decides what happens to a tag that already means something else.
	onConflict ociv1alpha1.TagConflictPolicy
}

// target is where this object publishes: the repository it named, or the operator's default,
// under the object's own name (ADR 0035).
func (r *ImageCompositionReconciler) target(obj *ociv1alpha1.ImageComposition) (target, error) {
	p := obj.Spec.Push
	repo := ""
	if p != nil {
		repo = p.Repository
	}
	if repo == "" {
		if !r.Default.Configured() {
			// Pending, not Stalled: the fix is restarting the controller with a registry, which
			// bumps no generation here.
			return target{}, recon.Pending(
				"this object names no repository, and no default registry is configured")
		}
		repo = r.Default.RepositoryFor(obj.Namespace, obj.Name)
	}

	tags, err := recon.EffectiveTags(p.GetTags(), p.GetRef())
	if err != nil {
		return target{}, err
	}
	return target{
		writeRepo:  repo,
		pullRepo:   r.Default.PublicRepository(repo),
		tags:       tags,
		onConflict: p.ResolveConflictPolicy(),
	}, nil
}

// refOptions decides whether this repository may be reached over plain HTTP.
//
// A method so unit tests can check it directly: go-containerregistry treats localhost as insecure
// on its own, so a push test against httptest passes whether or not this is consulted.
func (r *ImageCompositionReconciler) refOptions(repository string) []name.Option {
	if recon.InsecureHost(repository, r.InsecureRegistries) {
		return []name.Option{name.Insecure}
	}
	return nil
}

// artifactStatus reports the PULL reference, never the one the controller wrote to. Every field is
// anchored on the digest; a build with no tags reports a digest-only reference.
//
// own adds the digest's own tag to Tags, for content this pass actually named. It never moves
// Revision or Ref, which are built from the spec's first tag.
func artifactStatus(t target, digest v1.Hash, own bool) *ociv1alpha1.ArtifactStatus {
	now := metav1.Now()
	st := &ociv1alpha1.ArtifactStatus{
		Digest:         digest.String(),
		Revision:       digest.String(),
		Ref:            fmt.Sprintf("%s@%s", t.pullRepo, digest),
		LastUpdateTime: &now,
	}
	if len(t.tags) > 0 {
		st.Revision = fmt.Sprintf("%s@%s", t.tags[0], digest)
		st.Ref = fmt.Sprintf("%s:%s@%s", t.pullRepo, t.tags[0], digest)
		st.Tags = make([]string, 0, len(t.tags)+1)
		for _, tag := range t.tags {
			st.Tags = append(st.Tags, fmt.Sprintf("%s:%s", t.pullRepo, tag))
		}
	}
	if own {
		st.Tags = append(st.Tags, fmt.Sprintf("%s:%s", t.pullRepo, recon.DigestTag(digest.String())))
	}
	return st
}

// backfillDigestTags gives content published before ADR 0060 its digest's own tag.
//
// Driven by status, so it costs nothing once every record claims the tag. art is updated in place;
// the tagged history digests are returned for the status patch to mark. History too, because that
// is what a rollback pulls.
//
// Best effort: a failure is logged and retried next pass, and expired content stays unclaimed.
func (r *ImageCompositionReconciler) backfillDigestTags(
	ctx context.Context, obj *ociv1alpha1.ImageComposition, tgt target,
	art *ociv1alpha1.ArtifactStatus, refOpts []name.Option, opts []remote.Option,
) []string {
	logger := log.FromContext(ctx)
	applied := map[string]bool{}
	apply := func(digest string) bool {
		if done, seen := applied[digest]; seen {
			return done
		}
		err := recon.ApplyDigestTag(tgt.writeRepo, digest, refOpts, opts)
		if err != nil && !recon.IsNotFound(err) {
			logger.Error(err, "applying the digest's own tag; will retry", "digest", digest)
		}
		applied[digest] = err == nil
		return err == nil
	}

	if art.Digest != "" && !recon.HasDigestTag(art.Tags, art.Digest) && apply(art.Digest) {
		art.Tags = append(art.Tags, fmt.Sprintf("%s:%s", tgt.pullRepo, recon.DigestTag(art.Digest)))
	}
	var tagged []string
	for _, rec := range obj.Status.History {
		if rec.Digest != "" && !recon.HasDigestTag(rec.Tags, rec.Digest) && apply(rec.Digest) {
			tagged = append(tagged, rec.Digest)
		}
	}
	return tagged
}

// markDigestTagged records, on the history entries named, that their digest now carries its own
// tag. Bare, like the rest of a composition's history tags.
func markDigestTagged(history []ociv1alpha1.BuildRecord, digests []string) {
	for _, d := range digests {
		for i := range history {
			if history[i].Digest == d && !recon.HasDigestTag(history[i].Tags, d) {
				history[i].Tags = append(history[i].Tags, recon.DigestTag(d))
			}
		}
	}
}

// remoteOptions builds registry auth for writeRepo, the repository actually pushed to, so the
// credential is matched against the right host. Credentials come from a Secret, never the spec.
func (r *ImageCompositionReconciler) remoteOptions(
	ctx context.Context, obj *ociv1alpha1.ImageComposition, writeRepo string,
) ([]remote.Option, error) {
	return recon.RemoteAuth{
		Reader:    r.Client,
		Transport: r.Transport,
		Default:   r.Default,
	}.Options(ctx, obj.Namespace, writeRepo, obj.Spec.Push)
}

func configFrom(c *ociv1alpha1.ImageConfig) oci.Config {
	if c == nil {
		return oci.Config{}
	}
	return oci.Config{
		Inherit:      c.Inherit,
		Labels:       c.Labels,
		Env:          c.Env,
		Entrypoint:   c.Entrypoint,
		Cmd:          c.Cmd,
		User:         c.User,
		WorkingDir:   c.WorkingDir,
		ExposedPorts: c.ExposedPorts,
		Volumes:      c.Volumes,
		StopSignal:   c.StopSignal,
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func reasonFor(err error) string {
	switch {
	case strings.Contains(err.Error(), "digest mismatch"):
		return ociv1alpha1.ReasonDigestMismatch
	case strings.Contains(err.Error(), "already resolves to"):
		return ociv1alpha1.ReasonImmutableConflict
	default:
		return ociv1alpha1.ReasonInvalidSpec
	}
}

// exportRef publishes the reference into the ConfigMap a consumer substitutes from, and records
// where it went.
//
// ADR 0056.
func (r *ImageCompositionReconciler) exportRef(
	ctx context.Context, obj *ociv1alpha1.ImageComposition, art *ociv1alpha1.ArtifactStatus,
) error {
	spec := obj.Spec.Push.GetWriteRefTo()
	if spec == nil && obj.Status.RefExport == nil {
		return nil
	}

	var written *ociv1alpha1.RefExportStatus
	if spec != nil {
		if art == nil {
			return nil
		}
		var err error
		written, err = recon.ExportRef(ctx, r.Client, obj, spec, r.Export, art.Digest, art.Ref)
		if err != nil {
			return err
		}
	}

	changed, err := recon.RecordExport(ctx, r.Client, obj, obj.Status.RefExport, written)
	if err != nil || !changed {
		return err
	}
	return r.patchStatus(ctx, obj, func(o *ociv1alpha1.ImageComposition) {
		o.Status.RefExport = written
	})
}

// finalize removes the finalizer. Published artifacts are left in place, since running workloads
// may still use them. A ref export in ANOTHER namespace is deleted here; one in this namespace is
// owned by the object and garbage-collected.
func (r *ImageCompositionReconciler) finalize(ctx context.Context, obj *ociv1alpha1.ImageComposition) (ctrl.Result, error) {
	if err := recon.DeleteExportedRef(ctx, r.Client, obj, obj.Status.RefExport); err != nil {
		return ctrl.Result{}, err
	}
	patch := client.MergeFrom(obj.DeepCopy())
	controllerutil.RemoveFinalizer(obj, ociv1alpha1.Finalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, obj, patch))
}

func (r *ImageCompositionReconciler) patchStatus(ctx context.Context, obj *ociv1alpha1.ImageComposition, mutate func(*ociv1alpha1.ImageComposition)) error {
	key := client.ObjectKeyFromObject(obj)
	var latest ociv1alpha1.ImageComposition
	if err := r.Get(ctx, key, &latest); err != nil {
		return client.IgnoreNotFound(err)
	}
	patch := client.MergeFrom(latest.DeepCopy())
	mutate(&latest)
	// Set on EVERY status write, as Flux does: both describe the pass, not its outcome. Echoing only
	// on success makes `flux reconcile` time out on a failure, and kstatus read it as in progress.
	latest.Status.ObservedGeneration = latest.Generation
	latest.Status.LastHandledReconcileAt = latest.Annotations[ociv1alpha1.ReconcileRequestAnnotation]
	return r.Status().Patch(ctx, &latest, patch)
}

// compositionsForConfigMap maps a changed ConfigMap to the compositions that reference it.
func (r *ImageCompositionReconciler) compositionsForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	var list ociv1alpha1.ImageCompositionList
	// Namespace-scoped: a configMapRef resolves in the composition's own namespace, so a
	// same-named ConfigMap elsewhere is unrelated and must not trigger a rebuild.
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "could not map a ConfigMap change to compositions")
		return nil
	}

	var out []reconcile.Request
	for i := range list.Items {
		item := &list.Items[i]
		for _, l := range item.Spec.Layers {
			if l.ConfigMap != nil && l.ConfigMap.Name == obj.GetName() {
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name},
				})
				break
			}
		}
	}
	return out
}

// compositionsForSource maps a changed Flux source of one kind to the compositions referencing it.
//
// Lists cluster-wide because sourceRef carries its own namespace; the namespace comparison keeps
// same-named sources elsewhere from matching.
//
// The kind is captured rather than read off the object, because an unstructured object from a
// cache may not carry its GVK.
func (r *ImageCompositionReconciler) compositionsForSource(kind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list ociv1alpha1.ImageCompositionList
		if err := r.List(ctx, &list); err != nil {
			log.FromContext(ctx).Error(err, "could not map a source change to compositions", "kind", kind)
			return nil
		}

		var out []reconcile.Request
		for i := range list.Items {
			item := &list.Items[i]
			for _, l := range item.Spec.Layers {
				ref := l.SourceRef
				if ref == nil || ref.Kind != kind || ref.Name != obj.GetName() {
					continue
				}
				ns := ref.Namespace
				if ns == "" {
					ns = item.Namespace
				}
				if ns != obj.GetNamespace() {
					continue
				}
				out = append(out, reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name},
				})
				break
			}
		}
		return out
	}
}

// fluxSourceKinds are the kinds a sourceRef layer can name — the CRD's enum, and the same list the
// RBAC above grants read access to.
var fluxSourceKinds = []string{"GitRepository", "OCIRepository", "Bucket"}

// fluxSourceGroup is the group the source kinds live in.
const fluxSourceGroup = "source.toolkit.fluxcd.io"

// watchableSourceKinds returns the source kinds this cluster actually serves, paired with the
// version the API server prefers for each.
//
// Flux is NOT a dependency (ADR 0009), so a kind the RESTMapper cannot resolve is skipped. The
// mapper is consulted once, at startup: installing Flux later leaves no source watches until a
// restart, which only slows convergence to spec.interval (correctness is the resolver's, ADR 0026).
func watchableSourceKinds(mapper meta.RESTMapper) []schema.GroupVersionKind {
	var out []schema.GroupVersionKind
	for _, kind := range fluxSourceKinds {
		mapping, err := mapper.RESTMapping(schema.GroupKind{Group: fluxSourceGroup, Kind: kind})
		if err != nil {
			continue
		}
		out = append(out, mapping.GroupVersionKind)
	}
	return out
}

// SetupWithManager wires the controller up.
func (r *ImageCompositionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Fetcher == nil {
		r.Fetcher = oci.NewFetcher()
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&ociv1alpha1.ImageComposition{}).
		// So a ConfigMap edit rebuilds now rather than at the next interval.
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.compositionsForConfigMap))

	// Likewise for Flux sources: when a source and a composition change in one apply, the
	// composition waits (Pending) for the source to catch up, and this watch ends that wait.
	logger := mgr.GetLogger().WithName("imagecomposition")
	for _, gvk := range watchableSourceKinds(mgr.GetRESTMapper()) {
		src := &unstructured.Unstructured{}
		src.SetGroupVersionKind(gvk)
		b = b.Watches(src, handler.EnqueueRequestsFromMapFunc(r.compositionsForSource(gvk.Kind)))
		logger.Info("watching Flux source kind", "gvk", gvk.String())
	}

	return b.Complete(r)
}

// refreshNow renews the lease on what was just published, without waiting for the next cycle.
//
// Until this runs the artifact has NO lease: zot can carry an OLD timestamp onto a new tag for a
// digest it has seen before, so a collection pass in the gap could reclaim minutes-old content.
//
// Never fatal: the next scheduled cycle tries again.
func (r *ImageCompositionReconciler) refreshNow(
	ctx context.Context, obj *ociv1alpha1.ImageComposition, artifact *ociv1alpha1.ArtifactStatus,
) {
	if r.Refresher == nil || artifact == nil {
		return
	}
	// artifact is passed because obj.Status still describes the previous pass (patchStatus writes
	// to a fresh copy). History is left out: only this publish is newly unprotected.
	r.Refresher.RefreshNow(ctx, retention.Target{
		Object: obj, Push: obj.Spec.Push, Artifact: artifact,
	})
}
