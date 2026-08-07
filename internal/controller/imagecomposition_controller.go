package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
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
	"k8s.io/apimachinery/pkg/runtime"
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
	"github.com/lhns/kube-oci-composer/internal/serve"
)

// ReconcileRequestAnnotation matches Flux's, so `flux reconcile` and `kubectl annotate` both
// trigger a reconciliation the way users of the ecosystem expect.
const ReconcileRequestAnnotation = "reconcile.fluxcd.io/requestedAt"

// terminalError marks a failure that retrying cannot fix. It maps to Stalled rather than a
// backoff loop: a wrong digest or an invalid spec needs a human, and hammering the API server
// about it only hides the problem.
//
// The bar is narrow, and deliberately so: editing THIS object's spec must be what fixes it.
// That is what makes stalling safe, because the generation change is the wake-up. If the fix
// lives in another object, use pending instead.
type terminalError struct{ err error }

func (t *terminalError) Error() string { return t.err.Error() }
func (t *terminalError) Unwrap() error { return t.err }

func terminal(format string, a ...any) error {
	return &terminalError{err: fmt.Errorf(format, a...)}
}

// pendingError marks a dependency that is absent or unusable: a Flux source, a Secret, a
// non-optional ConfigMap, a serving endpoint the operator was never configured with.
//
// Neither terminal nor an ordinary transient failure. Not terminal, because each of these is
// fixed by changing a DIFFERENT object, which does not bump this object's generation — stalling
// would wait for an event that never arrives. Not an ordinary failure either, because "the
// GitRepository applied one second after me does not exist yet" is a normal step in converging
// a commit, not something to log as an error, raise a Warning about, and back off exponentially
// over.
//
// So it reports Reconciling with ReasonDependencyNotReady and retries on a short fixed interval.
type pendingError struct{ err error }

func (p *pendingError) Error() string { return p.err.Error() }
func (p *pendingError) Unwrap() error { return p.err }

func pending(format string, a ...any) error {
	return &pendingError{err: fmt.Errorf(format, a...)}
}

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
			setCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
				ociv1alpha1.ReasonSuspended, "Reconciliation is suspended")
			removeCondition(o, ociv1alpha1.ReconcilingCondition)
			removeCondition(o, ociv1alpha1.StalledCondition)
		})
	}

	// The CRD defaults this, so it is normally set. The fallback covers an object created before
	// the default existed, and a deliberate zero.
	interval := time.Hour
	if obj.Spec.Interval != nil && obj.Spec.Interval.Duration > 0 {
		interval = obj.Spec.Interval.Duration
	}

	result, err := r.reconcileArtifact(ctx, &obj)
	if err != nil {
		// Checked before terminal: a dependency that is not there yet is a normal step in
		// converging a commit, so it gets a quiet fixed-interval retry rather than a Warning
		// event, an error log and exponential backoff. Crucially it never sets Stalled — the
		// object that would fix it is a different one, and changing it raises no event here.
		var pe *pendingError
		if errors.As(err, &pe) {
			logger.Info("waiting on a dependency; will retry", "reason", err.Error(),
				"retryIn", pendingRetryInterval)
			return ctrl.Result{RequeueAfter: pendingRetryInterval},
				r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
					setCondition(o, ociv1alpha1.ReconcilingCondition, metav1.ConditionTrue,
						ociv1alpha1.ReasonDependencyNotReady, err.Error())
					setCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
						ociv1alpha1.ReasonDependencyNotReady, err.Error())
					removeCondition(o, ociv1alpha1.StalledCondition)
				})
		}

		var te *terminalError
		if errors.As(err, &te) {
			logger.Error(err, "terminal error; not retrying until the spec changes")
			r.event(&obj, corev1.EventTypeWarning, reasonFor(err), err.Error())
			// No requeue: Stalled means a human must act. The generation change that fixes it
			// wakes the controller anyway.
			return ctrl.Result{}, r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
				setCondition(o, ociv1alpha1.StalledCondition, metav1.ConditionTrue, reasonFor(err), err.Error())
				setCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse, reasonFor(err), err.Error())
				removeCondition(o, ociv1alpha1.ReconcilingCondition)
			})
		}

		logger.Error(err, "transient failure; will retry")
		r.event(&obj, corev1.EventTypeWarning, ociv1alpha1.ReasonFetchFailed, err.Error())
		if perr := r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
			setCondition(o, ociv1alpha1.ReconcilingCondition, metav1.ConditionTrue,
				ociv1alpha1.ReasonProgressing, err.Error())
			setCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
				ociv1alpha1.ReasonProgressing, err.Error())
			removeCondition(o, ociv1alpha1.StalledCondition)
		}); perr != nil {
			return ctrl.Result{}, perr
		}
		// Returning the error lets controller-runtime apply exponential backoff.
		return ctrl.Result{}, err
	}

	if err := r.patchStatus(ctx, &obj, func(o *ociv1alpha1.ImageComposition) {
		o.Status.Artifact = result.Artifact
		o.Status.InputHash = result.InputHash
		o.Status.History = recordHistory(o.Status.History, result.Record, r.historyLimit(o))
		o.Status.ObservedGeneration = o.Generation
		o.Status.LastHandledReconcileAt = o.Annotations[ReconcileRequestAnnotation]
		setCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonSucceeded, fmt.Sprintf("Published %s", result.Artifact.Ref))
		removeCondition(o, ociv1alpha1.ReconcilingCondition)
		removeCondition(o, ociv1alpha1.StalledCondition)
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
func buildRecord(art builtArtifact, tags []string, digest v1.Hash) (*ociv1alpha1.BuildRecord, error) {
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
		Time:      &now,
	}, nil
}

// publishedState is what the registry currently holds for this target: what each tag resolves
// to, and whether the digest recorded in status is still present.
//
// Gathered in one place because two separate decisions depend on it — whether there is anything
// to do, and whether doing it would remean a tag — and both must agree about what is out there.
type publishedState struct {
	// tags maps tag -> the digest it resolves to. Absent means the tag does not exist.
	tags map[string]string
	// wanted is how many tags were asked for, so a missing one is distinguishable from a
	// wrong one.
	wanted int
	// digest is the recorded digest, present iff it still resolves.
	digest string
}

// matches reports whether the given digest is fully published: present by digest, and every
// requested tag already pointing at it. With no tags that is just "the content is there".
func (p publishedState) matches(digest string) bool {
	if digest == "" || p.digest != digest {
		return false
	}
	for _, cur := range p.tags {
		if cur != digest {
			return false
		}
	}
	return len(p.tags) == p.wanted
}

func (r *ImageCompositionReconciler) resolvePublished(
	tgt target, prev *ociv1alpha1.ArtifactStatus, refOpts []name.Option, opts []remote.Option,
) (publishedState, error) {
	state := publishedState{tags: make(map[string]string, len(tgt.tags)), wanted: len(tgt.tags)}

	for _, tag := range tgt.tags {
		ref, err := name.ParseReference(fmt.Sprintf("%s:%s", tgt.writeRepo, tag), refOpts...)
		if err != nil {
			return publishedState{}, terminal("invalid reference %s:%s: %v", tgt.writeRepo, tag, err)
		}
		// A HEAD failure is not an error: the usual cause is that the tag does not exist yet.
		if desc, err := remote.Head(ref, opts...); err == nil {
			state.tags[tag] = desc.Digest.String()
		}
	}

	// The digest has to be checked separately rather than inferred from the tags, because a
	// build with no tags has nothing else to go on — and because a tag resolving correctly does
	// not prove the digest reference itself survived a storage wipe.
	if prev != nil && prev.Digest != "" {
		ref, err := name.ParseReference(fmt.Sprintf("%s@%s", tgt.writeRepo, prev.Digest), refOpts...)
		if err == nil {
			if _, err := remote.Head(ref, opts...); err == nil {
				state.digest = prev.Digest
			}
		}
	}
	return state, nil
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

	inputs, err := r.resolveInputs(ctx, obj, workDir)
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
		baseDigest = obj.Spec.Base.Digest
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
	published, err := r.resolvePublished(tgt, obj.Status.Artifact, refOpts, opts)
	if err != nil {
		return buildResult{}, err
	}

	// The cheap path. Same inputs, and everything those inputs produced last time is still
	// published under every name it should be, so there is nothing to do. This is what makes
	// reconciling on an interval nearly free: without it, the output digest could only be
	// learned by downloading every layer and assembling them, every hour, forever.
	if prev := obj.Status.Artifact; prev != nil &&
		obj.Status.InputHash == inputHash &&
		published.matches(prev.Digest) {
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

		path, err := r.resolveLayer(ctx, inputs[i])
		if err != nil {
			var dm *oci.ErrDigestMismatch
			if errors.As(err, &dm) {
				// Terminal on purpose: the declared digest and the served bytes disagree, and
				// no amount of retrying reconciles that. Retrying would also mean repeatedly
				// pulling content we have already decided not to trust.
				return buildResult{}, terminal("layer %q: %s", inputs[i].Name, dm.Error())
			}
			return buildResult{}, fmt.Errorf("layer %q: %w", inputs[i].Name, err)
		}
		inputs[i].Path = path
	}

	art, err := r.assemble(ctx, obj, declared, inputs, cfg, workDir)
	if err != nil {
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
	if published.matches(digest.String()) {
		return buildResult{Artifact: artifactStatus(tgt, digest), InputHash: inputHash}, nil
	}

	if tgt.immutable {
		// Refuse to change what a tag means. Terminal, because that is the failure mode which
		// leaves nodes running different bytes under one name, and no amount of retrying fixes
		// it. Checked before anything is written, so a partial rename cannot happen.
		for _, tag := range tgt.tags {
			if cur, ok := published.tags[tag]; ok && cur != digest.String() {
				return buildResult{}, terminal(
					"tag %s already resolves to %s but this spec produces %s; change the tag, or set immutable: false if it is meant to move",
					tag, cur, digest)
			}
		}
	}

	// The digest reference first. It is the one thing that is always correct, it is what image
	// automation pins, and writing it before any tag means a failure part-way through leaves the
	// content addressable rather than a tag pointing at nothing.
	digestRef, err := name.ParseReference(fmt.Sprintf("%s@%s", tgt.writeRepo, digest), refOpts...)
	if err != nil {
		return buildResult{}, terminal("invalid reference %s@%s: %v", tgt.writeRepo, digest, err)
	}
	if err := art.write(digestRef, opts...); err != nil {
		return buildResult{}, fmt.Errorf("publishing %s: %w", digestRef, err)
	}

	for _, tag := range tgt.tags {
		ref, err := name.ParseReference(fmt.Sprintf("%s:%s", tgt.writeRepo, tag), refOpts...)
		if err != nil {
			return buildResult{}, terminal("invalid reference %s:%s: %v", tgt.writeRepo, tag, err)
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

	r.event(obj, corev1.EventTypeNormal, ociv1alpha1.ReasonSucceeded,
		fmt.Sprintf("Published %s@%s%s", tgt.pullRepo, digest, tagSuffix(tgt.tags)))

	record, err := buildRecord(art, tgt.tags, digest)
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

// DefaultHistoryLimit is how many past builds are retained when nothing says otherwise.
//
// Not 1, and not unbounded. Layers are shared between builds so the marginal cost of retaining
// one is small, while the cost of having reclaimed one too eagerly is a workload that cannot pull
// the digest it is pinned to. See ADR 0011.
const DefaultHistoryLimit = 10

// historyLimit resolves the retention count for one object.
func (r *ImageCompositionReconciler) historyLimit(obj *ociv1alpha1.ImageComposition) int {
	if obj.Spec.Publish != nil && obj.Spec.Publish.History != nil {
		return int(*obj.Spec.Publish.History)
	}
	if r.HistoryLimit > 0 {
		return r.HistoryLimit
	}
	return DefaultHistoryLimit
}

// recordHistory prepends a new build and trims to the limit.
//
// A nil record means the reconcile converged without publishing, and must not touch history.
// Appending on every interval would fill the retention list with duplicates of the current build
// and evict genuinely distinct older ones within hours, quietly breaking the guarantee retention
// exists to provide.
func recordHistory(history []ociv1alpha1.BuildRecord, record *ociv1alpha1.BuildRecord, limit int) []ociv1alpha1.BuildRecord {
	if record == nil {
		return history
	}
	if limit < 1 {
		limit = 1
	}

	// A rebuild that reproduces an earlier digest moves that entry to the front rather than
	// duplicating it. Reverting a change and reverting it back is ordinary, and each round trip
	// would otherwise burn two retention slots on one distinct artifact.
	out := make([]ociv1alpha1.BuildRecord, 0, limit)
	out = append(out, *record)
	for _, h := range history {
		if h.Digest == record.Digest {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, h)
	}
	return out
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
	// immutable refuses to remean an existing tag rather than moving it.
	immutable bool
	// insecure allows plaintext HTTP, true only for the loopback endpoint.
	insecure bool
}

// tagPattern is the CRD's own constraint on a tag, applied here too because a tag arriving via
// publish.ref never passed through that validation.
var tagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]*$`)

// tagFromRef extracts the tag from a full image reference, and NOTHING else — the host and
// repository are the caller's business, not this field's.
//
// Deliberately hand-parsed rather than handed to name.ParseReference, which would default a bare
// "my-artifact" to "index.docker.io/library/my-artifact:latest". Inventing a `latest` out of an
// untemplated placeholder is exactly the wrong answer: it would publish a moving tag nobody asked
// for. No tag in, no tag out.
func tagFromRef(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if strings.ContainsRune(ref, '@') {
		return "", terminal("publish.ref %q carries a digest; it must name a tag, since the digest is an output rather than an input", ref)
	}
	// A colon before the last slash is a port, not a tag: "registry:5000/repo".
	colon := strings.LastIndexByte(ref, ':')
	if colon <= strings.LastIndexByte(ref, '/') {
		return "", nil
	}
	tag := ref[colon+1:]
	if !tagPattern.MatchString(tag) {
		return "", terminal("publish.ref %q has an invalid tag %q", ref, tag)
	}
	return tag, nil
}

// effectiveTags is the explicit list plus whatever ref carries, in order and without duplicates.
func effectiveTags(tags []string, ref string) ([]string, error) {
	fromRef, err := tagFromRef(ref)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(tags)+1)
	seen := make(map[string]struct{}, len(tags)+1)
	for _, t := range append(append([]string(nil), tags...), fromRef) {
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out, nil
}

// resolve picks the publication target: an external registry when push is set, otherwise the
// built-in endpoint.
func (r *ImageCompositionReconciler) target(obj *ociv1alpha1.ImageComposition) (target, error) {
	if p := obj.Spec.Push; p != nil {
		return target{
			writeRepo: p.Repository,
			pullRepo:  p.Repository,
			tags:      p.Tags,
			immutable: p.TagsAreImmutable(),
		}, nil
	}
	if r.Server == nil {
		// Operator-level misconfiguration, not a spec error. Giving the operator a serving
		// endpoint means restarting it with different flags — which changes nothing about this
		// object, so stalling would leave every composition wedged after the fix. It waits.
		return target{}, pending("spec.push is unset and no serving endpoint is configured yet")
	}
	tags, err := effectiveTags(obj.Spec.Publish.GetTags(), obj.Spec.Publish.GetRef())
	if err != nil {
		return target{}, err
	}
	path := publishName(obj)
	return target{
		writeRepo: fmt.Sprintf("127.0.0.1%s/%s", r.Server.Addr, path),
		pullRepo:  fmt.Sprintf("%s/%s", r.Server.Host, path),
		tags:      tags,
		immutable: obj.Spec.Publish.TagsAreImmutable(),
		insecure:  true,
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

	p := obj.Spec.Push
	if p == nil || p.SecretRef == nil {
		return append(opts, remote.WithAuth(authn.Anonymous)), nil
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: obj.Namespace, Name: p.SecretRef.Name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			// See pullOptions: waits rather than stalls, for the same reason.
			return nil, pending("secret %s not found yet", key)
		}
		return nil, fmt.Errorf("reading secret %s: %w", key, err)
	}

	kc, err := keychainFromSecret(&secret)
	if err != nil {
		return nil, pending("secret %s is unusable: %v", key, err)
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

func (r *ImageCompositionReconciler) event(obj *ociv1alpha1.ImageComposition, kind, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, kind, reason, msg)
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
	return r.Status().Patch(ctx, &latest, patch)
}

func setCondition(o *ociv1alpha1.ImageComposition, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            truncate(msg, 32768),
		ObservedGeneration: o.Generation,
	})
}

func removeCondition(o *ociv1alpha1.ImageComposition, condType string) {
	meta.RemoveStatusCondition(&o.Status.Conditions, condType)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
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

// SetupWithManager wires the controller up.
func (r *ImageCompositionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Fetcher == nil {
		r.Fetcher = oci.NewFetcher()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&ociv1alpha1.ImageComposition{}).
		// Without this a ConfigMap edit would only be noticed at the next interval, which
		// defaults to an hour. Users reasonably expect editing the source of a layer to rebuild
		// it, and a silent hour of staleness reads as the controller being broken.
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.compositionsForConfigMap)).
		Complete(r)
}
