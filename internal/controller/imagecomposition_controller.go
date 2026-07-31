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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	"github.com/lhns/kube-oci-composer/internal/serve"
)

// ReconcileRequestAnnotation matches Flux's, so `flux reconcile` and `kubectl annotate` both
// trigger a reconciliation the way users of the ecosystem expect.
const ReconcileRequestAnnotation = "reconcile.fluxcd.io/requestedAt"

// terminalError marks a failure that retrying cannot fix. It maps to Stalled rather than a
// backoff loop: a wrong digest or an invalid spec needs a human, and hammering the API server
// about it only hides the problem.
type terminalError struct{ err error }

func (t *terminalError) Error() string { return t.err.Error() }
func (t *terminalError) Unwrap() error { return t.err }

func terminal(format string, a ...any) error {
	return &terminalError{err: fmt.Errorf(format, a...)}
}

// ImageCompositionReconciler assembles and publishes OCI artifacts.
type ImageCompositionReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Server is the built-in endpoint used when spec.push is unset.
	Server *serve.Server

	// Fetcher retrieves layer content.
	Fetcher *oci.Fetcher

	// Readiness gates the pod's readiness probe until the served store is warm. Optional; when
	// nil, readiness is not tracked.
	Readiness *Readiness
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
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch

func (r *ImageCompositionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var obj ociv1alpha1.ImageComposition
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		if apierrors.IsNotFound(err) && r.Readiness != nil {
			r.Readiness.Forget(req.NamespacedName)
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

	interval := obj.Spec.Interval.Duration
	if interval <= 0 {
		interval = time.Hour
	}

	art, inputHash, err := r.reconcileArtifact(ctx, &obj)
	if err != nil {
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
		o.Status.Artifact = art
		o.Status.InputHash = inputHash
		o.Status.ObservedGeneration = o.Generation
		o.Status.LastHandledReconcileAt = o.Annotations[ReconcileRequestAnnotation]
		setCondition(o, ociv1alpha1.ReadyCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonSucceeded, fmt.Sprintf("Published %s", art.Ref))
		removeCondition(o, ociv1alpha1.ReconcilingCondition)
		removeCondition(o, ociv1alpha1.StalledCondition)
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

// reconcileArtifact does the work and returns what is published.
//
// The ordering is the important part, and it is ordered by cost. The input hash is computed from
// the spec alone, so the cheapest possible check — one HEAD, no network transfer — comes first
// and covers the overwhelmingly common case of nothing having changed. Only past that point does
// anything get fetched, and only past the digest comparison does anything get written.
func (r *ImageCompositionReconciler) reconcileArtifact(ctx context.Context, obj *ociv1alpha1.ImageComposition) (*ociv1alpha1.ArtifactStatus, string, error) {
	// Built from the spec only; Path is filled in later, after we know a build is actually
	// needed. InputHash deliberately ignores Path for exactly this reason.
	inputs := make([]oci.LayerInput, 0, len(obj.Spec.Layers))
	for _, l := range obj.Spec.Layers {
		if l.URLSource == nil {
			// v0.1 supports url entries only. The union is already CEL-validated, so this is a
			// guard against a source kind the CRD allows but this build cannot handle.
			return nil, "", terminal("layer %q: only url sources are supported in this version", l.Name)
		}
		inputs = append(inputs, oci.LayerInput{
			Name:   l.Name,
			URL:    l.URL,
			Digest: l.Digest,
			Unpack: oci.UnpackMode(orDefault(string(l.Unpack), "none")),
			Target: orDefault(l.Target, "/"),
		})
	}

	cfg := configFrom(obj.Spec.Config)
	inputHash := oci.InputHash(inputs, cfg)

	tgt, err := r.target(obj)
	if err != nil {
		return nil, "", err
	}

	opts, err := r.remoteOptions(ctx, obj)
	if err != nil {
		return nil, "", err
	}

	// name.Insecure applies ONLY to the loopback serving endpoint. Applying it unconditionally
	// would silently downgrade pushes to a real registry to plaintext HTTP — the sort of quiet
	// weakening nobody notices until it matters.
	var refOpts []name.Option
	if tgt.insecure {
		refOpts = append(refOpts, name.Insecure)
	}

	movingRef, err := name.ParseReference(fmt.Sprintf("%s:%s", tgt.writeRepo, tgt.tag), refOpts...)
	if err != nil {
		return nil, "", terminal("invalid reference %s:%s: %v", tgt.writeRepo, tgt.tag, err)
	}

	// A HEAD failure is not an error here — the ordinary cause is that the tag does not exist,
	// or that the serving store was emptied by a restart.
	existing, headErr := remote.Head(movingRef, opts...)

	// The cheap path. Same inputs, and what is published is still exactly what those inputs
	// produced last time, so there is nothing to do. This is what makes reconciling on an
	// interval nearly free: without it, the output digest could only be learned by downloading
	// every layer and assembling them, every hour, forever.
	if prev := obj.Status.Artifact; prev != nil &&
		obj.Status.InputHash == inputHash &&
		headErr == nil && existing.Digest.String() == prev.Digest {
		return prev.DeepCopy(), inputHash, nil
	}

	workDir, err := os.MkdirTemp("", "oci-composer-work-*")
	if err != nil {
		return nil, "", fmt.Errorf("creating work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	for i := range inputs {
		path, err := r.Fetcher.FetchURL(ctx, inputs[i].URL, inputs[i].Digest)
		if err != nil {
			var dm *oci.ErrDigestMismatch
			if errors.As(err, &dm) {
				// Terminal on purpose: the declared digest and the served bytes disagree, and
				// no amount of retrying reconciles that. Retrying would also mean repeatedly
				// pulling content we have already decided not to trust.
				return nil, "", terminal("layer %q: %s", inputs[i].Name, dm.Error())
			}
			return nil, "", fmt.Errorf("layer %q: %w", inputs[i].Name, err)
		}
		inputs[i].Path = path
	}
	defer func() {
		for _, in := range inputs {
			if in.Path != "" {
				os.Remove(in.Path)
			}
		}
	}()

	img, err := oci.Assemble(inputs, cfg, workDir)
	if err != nil {
		return nil, "", terminal("assembling: %v", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, "", fmt.Errorf("computing digest: %w", err)
	}

	contentTag := fmt.Sprintf("%s-%s", tgt.tag, strings.TrimPrefix(digest.String(), "sha256:")[:12])

	// Second convergence check, now against the real output digest. The input hash can differ
	// while the output does not — a cosmetic spec change, or a controller that lost its recorded
	// hash — and there is no reason to republish identical bytes.
	if headErr == nil && existing.Digest == digest {
		return artifactStatus(tgt, contentTag, digest), inputHash, nil
	}

	if obj.Spec.Push != nil && obj.Spec.Push.Immutable && headErr == nil {
		// Opt-in guard for people using the tag as a version rather than a pointer. Terminal,
		// because silently changing what a tag means is the failure mode that leaves nodes
		// running different bytes under the same name. Checked before anything is written.
		return nil, "", terminal(
			"tag %s already resolves to %s but this spec produces %s; bump the tag or unset immutable",
			tgt.tag, existing.Digest, digest)
	}

	// The immutable content tag is written first and never reused, so the exact bytes stay
	// addressable even after the pointer moves on.
	contentRef, err := name.ParseReference(fmt.Sprintf("%s:%s", tgt.writeRepo, contentTag), refOpts...)
	if err != nil {
		return nil, "", terminal("invalid reference %s:%s: %v", tgt.writeRepo, contentTag, err)
	}
	if err := remote.Write(contentRef, img, opts...); err != nil {
		return nil, "", fmt.Errorf("publishing %s: %w", contentRef, err)
	}

	if err := remote.Write(movingRef, img, opts...); err != nil {
		return nil, "", fmt.Errorf("publishing %s: %w", movingRef, err)
	}

	r.event(obj, corev1.EventTypeNormal, ociv1alpha1.ReasonSucceeded,
		fmt.Sprintf("Published %s (%s)", contentRef, digest))

	return artifactStatus(tgt, contentTag, digest), inputHash, nil
}

// target is where an artifact is written and how it should be referenced. The two differ in
// serving mode: the controller writes over loopback but workloads pull via the Service.
type target struct {
	// writeRepo is what the controller pushes to.
	writeRepo string
	// pullRepo is what a workload references. Equal to writeRepo in push mode.
	pullRepo string
	// tag is the moving pointer.
	tag string
	// insecure allows plaintext HTTP, true only for the loopback endpoint.
	insecure bool
}

// resolve picks the publication target: an external registry when push is set, otherwise the
// built-in endpoint.
func (r *ImageCompositionReconciler) target(obj *ociv1alpha1.ImageComposition) (target, error) {
	if p := obj.Spec.Push; p != nil {
		return target{
			writeRepo: p.Repository,
			pullRepo:  p.Repository,
			tag:       orDefault(p.Tag, "latest"),
		}, nil
	}
	if r.Server == nil {
		return target{}, terminal("spec.push is unset but no serving endpoint is configured")
	}
	path := publishName(obj)
	tag := "latest"
	if obj.Spec.Publish != nil {
		tag = orDefault(obj.Spec.Publish.Tag, "latest")
	}
	return target{
		writeRepo: fmt.Sprintf("127.0.0.1%s/%s", r.Server.Addr, path),
		pullRepo:  fmt.Sprintf("%s/%s", r.Server.Host, path),
		tag:       tag,
		insecure:  true,
	}, nil
}

// artifactStatus reports the PULL reference, never the loopback one the controller wrote to.
// Getting that backwards would put an address into status that only the controller can reach.
func artifactStatus(t target, contentTag string, digest v1.Hash) *ociv1alpha1.ArtifactStatus {
	now := metav1.Now()
	return &ociv1alpha1.ArtifactStatus{
		Digest:         digest.String(),
		Revision:       fmt.Sprintf("%s@%s", t.tag, digest),
		Ref:            fmt.Sprintf("%s:%s@%s", t.pullRepo, t.tag, digest),
		ContentTag:     fmt.Sprintf("%s:%s", t.pullRepo, contentTag),
		LastUpdateTime: &now,
	}
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
			return nil, terminal("secret %s not found", key)
		}
		return nil, fmt.Errorf("reading secret %s: %w", key, err)
	}

	kc, err := keychainFromSecret(&secret)
	if err != nil {
		return nil, terminal("secret %s: %v", key, err)
	}
	return append(opts, remote.WithAuthFromKeychain(kc)), nil
}

func configFrom(c *ociv1alpha1.ImageConfig) oci.Config {
	if c == nil {
		return oci.Config{}
	}
	return oci.Config{Labels: c.Labels, Env: c.Env, Entrypoint: c.Entrypoint, Cmd: c.Cmd}
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

// SetupWithManager wires the controller up.
func (r *ImageCompositionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Fetcher == nil {
		r.Fetcher = oci.NewFetcher()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&ociv1alpha1.ImageComposition{}).
		Complete(r)
}
