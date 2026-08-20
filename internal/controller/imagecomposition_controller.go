package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/cache"
	"github.com/lhns/kube-oci-composer/internal/oci"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
	"github.com/lhns/kube-oci-composer/internal/serve"
)

// pendingRetryInterval is how often a composition waiting on a dependency re-checks. Short
// enough that a same-commit apply converges without anyone noticing; long enough that a
// genuinely missing reference costs a couple of cheap GETs a minute rather than a hot loop.
const pendingRetryInterval = 30 * time.Second

// ImageCompositionReconciler assembles and publishes OCI artifacts.
type ImageCompositionReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Server is the built-in endpoint used when spec.push is unset.
	Server *serve.Server

	// Default is where objects publish when they name no repository of their own. Configured once
	// by the operator; see recon.DefaultRegistry for why its credential is namespaced to the
	// controller rather than to the object.
	Default recon.DefaultRegistry

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

	// replay tracks which objects have had their published history restored into the registry
	// this process. See replay.go.
	replay replayer
}

// The controller never creates or deletes ImageCompositions — it only observes them and patches
// their finalizers and status. The verbs below say exactly that, so the role cannot quietly
// grant more than the code uses.
//
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagecompositions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagecompositions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagecompositions/finalizers,verbs=update
// Secrets are read by name and deliberately NOT cached (see cmd/oci-composer), so this needs
// `get` alone. Caching them would require list and watch on every Secret in the cluster —
// an enormous blast radius for a controller that reads one referenced push credential.
//
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
//
// ConfigMaps ARE cached, unlike Secrets, because configMapRef layers are watched — a ConfigMap
// edit must rebuild promptly rather than at the next interval, which could be an hour away.
// That costs an informer over all ConfigMaps; the alternative is a controller that appears not
// to notice edits.
//
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
//
// Flux sources are read for their status.artifact only. Read-only, and only the source kinds a
// layer can reference.
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
			r.replay.forget(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Recorded on the way out of every branch, so readiness reflects "this object has been
	// through a reconcile" rather than "this object is healthy". See Readiness.Observe.
	if r.Readiness != nil {
		defer r.Readiness.Observe(req.NamespacedName)
	}

	if !obj.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &obj)
	}

	if !controllerutilContainsFinalizer(&obj, ociv1alpha1.Finalizer) {
		patch := client.MergeFrom(obj.DeepCopy())
		obj.Finalizers = append(obj.Finalizers, ociv1alpha1.Finalizer)
		if err := r.Patch(ctx, &obj, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	// Suspend halts work without touching what is already published — the object simply stops
	// being reconciled, and says so.
	if obj.Spec.Suspend {
		return ctrl.Result{}, r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
			recon.SetCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
				ociv1alpha1.ReasonSuspended, "Reconciliation is suspended")
			recon.RemoveCondition(o, ociv1alpha1.ReconcilingCondition)
			recon.RemoveCondition(o, ociv1alpha1.StalledCondition)
		})
	}

	// The CRD defaults this, so it is normally set. The fallback covers an object created before
	// the default existed, and a deliberate zero.
	interval := recon.Interval(obj.Spec.Interval)

	result, err := r.reconcileArtifact(ctx, &obj)
	if err != nil {
		// Checked before terminal: a dependency that is not there yet is a normal step in
		// converging a commit, so it gets a quiet fixed-interval retry rather than a Warning
		// event, an error log and exponential backoff. Crucially it never sets Stalled — the
		// object that would fix it is a different one, and changing it raises no event here.
		var pe *recon.PendingError
		if errors.As(err, &pe) {
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

		var te *recon.TerminalError
		if errors.As(err, &te) {
			logger.Error(err, "terminal error; not retrying until the spec changes")
			recon.Event(r.Recorder, &obj, corev1.EventTypeWarning, reasonFor(err), err.Error())
			// No requeue: Stalled means a human must act. The generation change that fixes it
			// wakes the controller anyway.
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
		// Returning the error lets controller-runtime apply exponential backoff.
		return ctrl.Result{}, err
	}

	if err := r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
		o.Status.Artifact = result.Artifact
		o.Status.InputHash = result.InputHash
		o.Status.History = recon.RecordHistory(o.Status.History, result.Record, r.historyLimit(o))
		// Assigned unconditionally, including to nil: a divergence that has been resolved must stop
		// being reported, or the field becomes a permanent scar on an object that is now correct.
		o.Status.Conflict = result.Conflict
		msg := fmt.Sprintf("Published %s", result.Artifact.Ref)
		if c := result.Conflict; c != nil {
			// Ready, but the message says what was kept and what was dropped. A conflict an
			// operator has to go looking for is one they will not find.
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

	return ctrl.Result{RequeueAfter: interval}, nil
}

// buildResult is what one reconcile produced.
type buildResult struct {
	// Artifact is the published reference. Always set on success.
	Artifact *ociv1alpha1.ArtifactStatus
	// InputHash is the hash of everything that determined the output.
	InputHash string
	// Conflict is set when onConflict: Keep left an existing tag in place and dropped what this
	// reconcile produced. Copied into status so the divergence is visible rather than inferred.
	Conflict *ociv1alpha1.TagConflictStatus
	// Record describes a NEW build, and is nil when the reconcile converged without publishing.
	// Garbage collection reads status.history, so appending on a no-op would grow the retention
	// list with duplicates and evict genuinely distinct builds.
	Record *ociv1alpha1.BuildRecord
}

// buildRecord captures the blobs a build is composed of, so garbage collection can tell what is
// still live without inferring it from what happens to be in storage.
//
// For a multi-platform build it walks every child: Blobs is the union of their configs and layers,
// and Manifests names the children themselves. Both matter to GC — the layers are shared between
// children and so appear once, while the configs differ per platform and would otherwise be
// reclaimed under a live index.
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

// sourceRecords is where each layer's content came from, so an artifact can be traced back to a
// revision without pulling it apart. Layers that carry no revision still record their digest: for a
// fetch that IS the identity, and an empty revision is honest about there being none.
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
// The ordering is the important part, and it is ordered by cost. The input hash is computed from
// the spec alone, so the cheapest possible check — one HEAD, no network transfer — comes first
// and covers the overwhelmingly common case of nothing having changed. Only past that point does
// anything get fetched, and only past the digest comparison does anything get written.
func (r *ImageCompositionReconciler) reconcileArtifact(ctx context.Context, obj *ociv1alpha1.ImageComposition) (buildResult, error) {
	// Built from the spec only; Path is filled in later, after we know a build is actually
	// needed. InputHash deliberately ignores Path for exactly this reason.
	// The work directory holds both the assembled layer tarballs and anything synthesised while
	// resolving a source, so it is created before resolution rather than after.
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

	// Platforms that can be known WITHOUT fetching anything. A declared list is known; an unset
	// one is the base's platform, which the base digest already pins, or — with no base — the
	// controller's own. Fetching the base here to learn its platform would defeat the point of a
	// hash that exists to avoid fetching.
	declared, err := declaredPlatforms(obj)
	if err != nil {
		return buildResult{}, err
	}
	hashPlatforms := declared
	var baseDigest string
	if obj.Spec.Base != nil {
		// From the accessor, not the field: `ref` and `image`+`digest` name the same base, so they
		// must hash the same. Reading .Digest directly would make a base spelled as a ref hash as
		// empty, and rewriting a spec from one spelling to the other would rebuild and republish
		// every artifact for no change in content.
		_, baseDigest = obj.Spec.Base.Repository()
	} else if len(hashPlatforms) == 0 {
		hashPlatforms = []oci.Platform{oci.RuntimePlatform()}
	}
	inputHash := oci.InputHash(inputs, cfg, baseDigest, hashPlatforms)

	tgt, err := r.target(obj)
	if err != nil {
		return buildResult{}, err
	}

	opts, err := r.remoteOptions(ctx, obj)
	if err != nil {
		return buildResult{}, err
	}

	// name.Insecure applies ONLY to the loopback serving endpoint. Applying it unconditionally
	// would silently downgrade pushes to a real registry to plaintext HTTP — the sort of quiet
	// weakening nobody notices until it matters.
	var refOpts []name.Option
	if tgt.insecure {
		refOpts = append(refOpts, name.Insecure)
	}

	// Restore previously published builds before checking convergence, so that after a restart
	// the references resolve from replayed state rather than looking absent and forcing a
	// rebuild of something already in the store.
	r.replayHistory(ctx, obj)

	// What each tag currently resolves to, plus whether the previously recorded digest is still
	// present at all. A HEAD failure is not an error: the ordinary cause is that the reference
	// does not exist yet, or that the serving store was emptied by a restart.
	published, err := recon.ResolvePublished(tgt.writeRepo, tgt.tags, obj.Status.Artifact, refOpts, opts)
	if err != nil {
		return buildResult{}, err
	}

	// The cheap path. Same inputs, and everything those inputs produced last time is still
	// published under every name it should be, so there is nothing to do. This is what makes
	// reconciling on an interval nearly free: without it, the output digest could only be
	// learned by downloading every layer and assembling them, every hour, forever.
	if prev := obj.Status.Artifact; prev != nil &&
		obj.Status.InputHash == inputHash &&
		published.Matches(prev.Digest) {
		return buildResult{Artifact: prev.DeepCopy(), InputHash: inputHash}, nil
	}

	for i := range inputs {
		// Content synthesised during resolution (a ConfigMap) is already on disk; only remote
		// sources need fetching.
		if inputs[i].Path != "" {
			continue
		}

		// A remove entry has no content to fetch; it produces whiteouts from the spec alone.
		if len(inputs[i].Remove) > 0 {
			continue
		}

		// An image layer resolves to a manifest rather than a file. Pulled HERE rather than during
		// resolution, so that the short-circuit above can decide there is nothing to do without
		// touching a registry. It deliberately skips the layer cache, which is keyed by digest for
		// single blobs where an image is a manifest plus its layers — the registry is the cache.
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
				// Terminal on purpose: the declared digest and the served bytes disagree, and
				// no amount of retrying reconciles that. Retrying would also mean repeatedly
				// pulling content we have already decided not to trust.
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
			// Terminal on purpose: retrying cannot add a code path to this binary. See
			// oci.ErrUnsupportedUnpack for how a spec gets past the CRD's enum in the first place.
			return buildResult{}, recon.Terminal("%s", unsupported.Error())
		}
		return buildResult{}, err
	}

	digest, err := art.Digest()
	if err != nil {
		return buildResult{}, fmt.Errorf("computing digest: %w", err)
	}

	// Second convergence check, now against the real output digest. The input hash can differ
	// while the output does not — a cosmetic spec change, or a controller that lost its recorded
	// hash — and there is no reason to republish identical bytes.
	//
	// This runs BEFORE the immutability guard, and the order is load-bearing: republishing the
	// same content under the same tag has to stay a no-op, or a steady reconcile loop would fail
	// every time round with immutable tags.
	if published.Matches(digest.String()) {
		return buildResult{Artifact: artifactStatus(tgt, digest), InputHash: inputHash}, nil
	}

	// Checked before anything is written, so a partial rename cannot happen.
	if tag, cur := published.Conflicts(tgt.tags, digest.String()); tag != "" {
		switch tgt.onConflict {
		case ociv1alpha1.ConflictFail:
			// Refuse to change what a tag means. Terminal, because that is the failure mode which
			// leaves nodes running different bytes under one name, and no amount of retrying fixes
			// it.
			return buildResult{}, recon.Terminal(
				"tag %s already resolves to %s but this spec produces %s; change the tag, or set "+
					"onConflict: Overwrite if it is meant to move", tag, cur, digest)
		case ociv1alpha1.ConflictKeep:
			// Leave the tag alone and publish nothing. Ready, because with a spec-hash tag an
			// existing tag means the content is already published and correct.
			//
			// status.artifact reports the EXISTING digest, not the one just produced: it must
			// describe what a consumer actually pulls, and under Keep that is the content that was
			// already there. The digest this spec produced is recorded separately, because
			// otherwise the object would read healthy while nothing said the two had diverged --
			// precisely the shape of the incident behind ADR 0026.
			existing, err := v1.NewHash(cur)
			if err != nil {
				return buildResult{}, recon.Terminal(
					"tag %s resolves to %q, which is not a digest: %v", tag, cur, err)
			}
			now := metav1.Now()
			return buildResult{
				Artifact:  artifactStatus(tgt, existing),
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

	// The digest reference first. It is the one thing that is always correct, it is what image
	// automation pins, and writing it before any tag means a failure part-way through leaves the
	// content addressable rather than a tag pointing at nothing.
	digestRef, err := name.ParseReference(fmt.Sprintf("%s@%s", tgt.writeRepo, digest), refOpts...)
	if err != nil {
		return buildResult{}, recon.Terminal("invalid reference %s@%s: %v", tgt.writeRepo, digest, err)
	}
	if err := art.write(digestRef, opts...); err != nil {
		return buildResult{}, fmt.Errorf("publishing %s: %w", digestRef, err)
	}

	for _, tag := range tgt.tags {
		ref, err := name.ParseReference(fmt.Sprintf("%s:%s", tgt.writeRepo, tag), refOpts...)
		if err != nil {
			return buildResult{}, recon.Terminal("invalid reference %s:%s: %v", tgt.writeRepo, tag, err)
		}
		if err := art.write(ref, opts...); err != nil {
			return buildResult{}, fmt.Errorf("publishing %s: %w", ref, err)
		}
	}

	// Record the manifest so this build can be replayed after a restart. Not fatal: the artifact
	// is published and pullable right now, and losing replayability is a smaller failure than
	// reporting a build that actually succeeded as failed.
	//
	// For an index this stores the children too — an index alone would replay into a reference
	// that resolves but cannot be pulled.
	if r.Server != nil && obj.Spec.Push == nil {
		if sErr := art.saveManifests(ctx, r.Server.SaveManifest); sErr != nil {
			log.FromContext(ctx).Error(sErr, "could not persist the manifest; a restart will lose this build")
		}
	}

	recon.Event(r.Recorder, obj, corev1.EventTypeNormal, ociv1alpha1.ReasonSucceeded,
		fmt.Sprintf("Published %s@%s%s", tgt.pullRepo, digest, tagSuffix(tgt.tags)))

	record, err := buildRecord(art, tgt.tags, digest, inputs)
	if err != nil {
		// The artifact is published and usable; only the retention record is missing. Failing
		// here would leave storage holding blobs that nothing records as live, which is worse
		// than reporting the build and logging the gap.
		return buildResult{}, fmt.Errorf("recording build %s: %w", digest, err)
	}

	return buildResult{
		Artifact:  artifactStatus(tgt, digest),
		InputHash: inputHash,
		Record:    record,
	}, nil
}

// historyLimit resolves the retention count for one object.
func (r *ImageCompositionReconciler) historyLimit(obj *ociv1alpha1.ImageComposition) int {
	if obj.Spec.Publish != nil && obj.Spec.Publish.History != nil {
		return int(*obj.Spec.Publish.History)
	}
	if r.HistoryLimit > 0 {
		return r.HistoryLimit
	}
	return ociv1alpha1.DefaultHistoryLimit
}

// resolveLayer returns a local path holding the layer's content.
//
// With a cache configured this is usually a hit and costs nothing; the fetch is the fallback.
// Note that the returned path is owned by the cache and must NOT be removed by the caller —
// deleting it would evict the entry that was just populated and guarantee a miss next time.
func (r *ImageCompositionReconciler) resolveLayer(ctx context.Context, in oci.LayerInput) (string, error) {
	fetch := func(ctx context.Context, digest string) (string, error) {
		return r.Fetcher.FetchURL(ctx, in.URL, digest)
	}
	if r.Cache == nil {
		return fetch(ctx, in.Digest)
	}
	return r.Cache.Path(ctx, in.Digest, fetch)
}

// target is where an artifact is written and how it should be referenced. The two differ in
// serving mode: the controller writes over loopback but workloads pull via the Service.
type target struct {
	// writeRepo is what the controller pushes to.
	writeRepo string
	// pullRepo is what a workload references. Equal to writeRepo in push mode.
	pullRepo string
	// tags are the tags to publish under, in order. Empty means publish by digest alone.
	tags []string
	// onConflict decides what happens to a tag that already means something else.
	onConflict ociv1alpha1.TagConflictPolicy
	// insecure allows plaintext HTTP, true only for the loopback endpoint.
	insecure bool
	// usesDefault marks a target the OBJECT did not choose, which is the only case where the
	// operator's own credential may be used. See recon.DefaultRegistry.CredentialFor.
	usesDefault bool
}

// resolve picks the publication target: an external registry when push is set, otherwise the
// built-in endpoint.
func (r *ImageCompositionReconciler) target(obj *ociv1alpha1.ImageComposition) (target, error) {
	if p := obj.Spec.Push; p != nil {
		return target{
			writeRepo:  p.Repository,
			pullRepo:   p.Repository,
			tags:       p.Tags,
			onConflict: p.ResolveConflictPolicy(),
		}, nil
	}
	if r.Server == nil {
		// No serving endpoint. Publish to the operator's default registry, which is what a chart
		// install configures and what makes a default deployment publish somewhere real without
		// anyone editing a spec.
		if r.Default.Configured() {
			tags, err := recon.EffectiveTags(obj.Spec.Publish.GetTags(), obj.Spec.Publish.GetRef())
			if err != nil {
				return target{}, err
			}
			repo := r.Default.RepositoryFor(obj.Namespace, publishName(obj))
			return target{
				writeRepo:   repo,
				pullRepo:    repo,
				tags:        tags,
				onConflict:  obj.Spec.Publish.ResolveConflictPolicy(),
				usesDefault: true,
			}, nil
		}

		// Operator-level misconfiguration, not a spec error. Configuring a registry means
		// restarting the controller with different flags — which changes nothing about this
		// object, so stalling would leave every composition wedged after the fix. It waits.
		return target{}, recon.Pending(
			"this object names no repository, and neither a default registry nor a serving " +
				"endpoint is configured yet")
	}
	tags, err := recon.EffectiveTags(obj.Spec.Publish.GetTags(), obj.Spec.Publish.GetRef())
	if err != nil {
		return target{}, err
	}
	path := publishName(obj)
	return target{
		writeRepo:  fmt.Sprintf("127.0.0.1%s/%s", r.Server.Addr, path),
		pullRepo:   fmt.Sprintf("%s/%s", r.Server.Host, path),
		tags:       tags,
		onConflict: obj.Spec.Publish.ResolveConflictPolicy(),
		insecure:   true,
	}, nil
}

// artifactStatus reports the PULL reference, never the loopback one the controller wrote to.
// Getting that backwards would put an address into status that only the controller can reach.
//
// The digest is the value that is always present and always correct, so it anchors every field
// here; tags decorate it. A build with no tags reports a digest-only reference rather than
// something with an empty tag in it.
func artifactStatus(t target, digest v1.Hash) *ociv1alpha1.ArtifactStatus {
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
		st.Tags = make([]string, 0, len(t.tags))
		for _, tag := range t.tags {
			st.Tags = append(st.Tags, fmt.Sprintf("%s:%s", t.pullRepo, tag))
		}
	}
	return st
}

func publishName(obj *ociv1alpha1.ImageComposition) string {
	if obj.Spec.Publish != nil && obj.Spec.Publish.Name != "" {
		return obj.Spec.Publish.Name
	}
	return obj.Name
}

// remoteOptions builds registry auth. Credentials are always read from a referenced Secret,
// never taken from the spec.
func (r *ImageCompositionReconciler) remoteOptions(ctx context.Context, obj *ociv1alpha1.ImageComposition) ([]remote.Option, error) {
	opts := []remote.Option{remote.WithContext(ctx)}

	var ownRef string
	if p := obj.Spec.Push; p != nil && p.SecretRef != nil {
		ownRef = p.SecretRef.Name
	}
	// usesDefault is what separates "the operator chose this registry" from "the tenant did", and
	// the operator's credential is only ever sent to the former.
	name, namespace := r.Default.CredentialFor(obj.Namespace, ownRef, obj.Spec.Push == nil)
	if name == "" {
		return append(opts, remote.WithAuth(authn.Anonymous)), nil
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			// See pullOptions: waits rather than stalls, for the same reason.
			return nil, recon.Pending("secret %s not found yet", key)
		}
		return nil, fmt.Errorf("reading secret %s: %w", key, err)
	}

	kc, err := recon.KeychainFromSecret(&secret)
	if err != nil {
		return nil, recon.Pending("secret %s is unusable: %v", key, err)
	}
	return append(opts, remote.WithAuthFromKeychain(kc)), nil
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

// finalize removes the finalizer. Published artifacts are deliberately left in place: they are
// content-addressed and may still be referenced by a running workload, so deleting the object
// that described them is not a reason to break pods that are using them.
func (r *ImageCompositionReconciler) finalize(ctx context.Context, obj *ociv1alpha1.ImageComposition) (ctrl.Result, error) {
	patch := client.MergeFrom(obj.DeepCopy())
	obj.Finalizers = removeString(obj.Finalizers, ociv1alpha1.Finalizer)
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
	// Set on EVERY status write, not just the successful one, because both describe the pass rather
	// than its outcome — which is how Flux writes them. Echoing only on success makes `flux
	// reconcile` wait for a token that never arrives and report a timeout instead of the failure the
	// object is already describing; and a stale observedGeneration reads to kstatus as "still
	// working" rather than "failed".
	latest.Status.ObservedGeneration = latest.Generation
	latest.Status.LastHandledReconcileAt = latest.Annotations[ociv1alpha1.ReconcileRequestAnnotation]
	return r.Status().Patch(ctx, &latest, patch)
}

func removeString(in []string, s string) []string {
	out := in[:0]
	for _, v := range in {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

func controllerutilContainsFinalizer(o client.Object, f string) bool {
	for _, v := range o.GetFinalizers() {
		if v == f {
			return true
		}
	}
	return false
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
// Cluster-wide, unlike the ConfigMap mapping: sourceRef carries an explicit namespace and is
// routinely pointed at a shared source in flux-system, so listing only the source's own namespace
// would miss exactly the arrangement most clusters use. The namespace comparison below is what
// keeps a same-named source elsewhere from triggering unrelated rebuilds.
//
// The kind is captured rather than read off the incoming object, because an unstructured object
// arriving from a cache has no guarantee of carrying its GVK, and silently matching every kind
// would make a Bucket edit rebuild a GitRepository-backed composition.
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

// fluxSourceGVK is the group the source kinds live in.
const fluxSourceGroup = "source.toolkit.fluxcd.io"

// watchableSourceKinds returns the source kinds this cluster actually serves, paired with the
// version the API server prefers for each.
//
// Flux is NOT a dependency (ADR 0009) and a cluster without it must work unchanged, so a kind the
// RESTMapper cannot resolve is skipped rather than treated as a startup failure. The cost of that
// is stated plainly: the mapper is consulted once, at startup, so installing Flux into a running
// cluster leaves this controller without source watches until it is restarted. It still reconciles
// those compositions on spec.interval, which is the pre-existing behaviour, so the failure mode is
// slowness rather than incorrectness — and correctness is now the resolver's job (ADR 0026), not
// the watch's.
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
		// Without this a ConfigMap edit would only be noticed at the next interval, which
		// defaults to an hour. Users reasonably expect editing the source of a layer to rebuild
		// it, and a silent hour of staleness reads as the controller being broken.
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.compositionsForConfigMap))

	// And the same argument for Flux sources, with an extra edge. A generator that bumps a
	// GitRepository's ref.tag and the composition's publish tag in ONE apply reconciles this object
	// immediately and the source not at all — so without a watch, the window during which the
	// composition is waiting for its source to catch up lasted until the next interval. The
	// resolver refuses to build in that window; this is what ends it in seconds.
	logger := mgr.GetLogger().WithName("imagecomposition")
	for _, gvk := range watchableSourceKinds(mgr.GetRESTMapper()) {
		src := &unstructured.Unstructured{}
		src.SetGroupVersionKind(gvk)
		b = b.Watches(src, handler.EnqueueRequestsFromMapFunc(r.compositionsForSource(gvk.Kind)))
		logger.Info("watching Flux source kind", "gvk", gvk.String())
	}

	return b.Complete(r)
}
