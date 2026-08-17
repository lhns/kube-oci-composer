package buildcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/build"
	"github.com/lhns/kube-oci-composer/internal/source"
)

// DockerBuildReconciler runs a Job per build and records what it produced.
//
// The reconcile is deliberately the composer's three-phase shape: resolve everything from the API
// without transferring anything, hash it, and only past that point do expensive work. That is what
// answers ADR 0001's objection — "the reconcile loop would have to rebuild to discover whether a
// rebuild was needed" — because every input here is resolvable from the API server. What it cannot
// do is the composer's SECOND check, against the real output digest, because there is nothing to
// compare against until a build has run. See ADR 0025.
type DockerBuildReconciler struct {
	client.Client
	JobConfig JobConfig

	// HistoryLimit is how many past builds are retained in status.
	HistoryLimit int
}

// +kubebuilder:rbac:groups=oci.lhns.de,resources=dockerbuilds,verbs=get;list;watch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=dockerbuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// Secrets are read for their resourceVersion only, so a rotation moves the input hash and
// rebuilds. The VALUE is never read here — it is projected straight into the build pod — which is
// why this is get and not list or watch, matching the composer's reasoning about blast radius.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories;ocirepositories;buckets,verbs=get;list;watch

func (r *DockerBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var obj ociv1alpha1.DockerBuild
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if obj.Spec.Suspend {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(obj.DeepCopy())
	result, err := r.reconcile(ctx, &obj)

	obj.Status.ObservedGeneration = obj.Generation
	r.applyOutcome(&obj, err)
	if perr := r.Status().Patch(ctx, &obj, patch); perr != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", perr)
	}

	switch {
	case err == nil:
		return result, nil
	case isTerminal(err):
		// Stalled. No requeue: the generation change from editing the spec is the wake-up.
		logger.Error(err, "stalled")
		return ctrl.Result{}, nil
	case isPending(err):
		return ctrl.Result{RequeueAfter: pendingRetryInterval}, nil
	default:
		// A build failure, or anything else transient. Capped backoff rather than exponential
		// forever, because the fix is usually a push to the Dockerfile's repository and the retry
		// is what notices it.
		logger.Error(err, "build failed", "failures", obj.Status.Failures)
		return ctrl.Result{RequeueAfter: failureBackoff(obj.Status.Failures)}, nil
	}
}

// reconcile is the state machine over the owned Job.
func (r *DockerBuildReconciler) reconcile(ctx context.Context, obj *ociv1alpha1.DockerBuild) (ctrl.Result, error) {
	inputs, contextURL, err := r.resolveInputs(ctx, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	inputHash := inputs.Hash()

	// The cheap path, and the whole point of hashing inputs: nothing has changed, so there is
	// nothing to do. Note what is NOT checked here — that the artifact is still present in the
	// registry. The composer verifies that with one HEAD because it can rebuild identical bytes if
	// it is gone; a rebuild here might not produce the same digest, so re-verifying would risk
	// turning a missing artifact into a permanent immutable-tag conflict. ADR 0025 records that
	// storage durability stops being optional for this kind.
	if obj.Status.Artifact != nil && obj.Status.InputHash == inputHash {
		return ctrl.Result{RequeueAfter: interval(obj)}, nil
	}

	// Adopt, observe or start.
	job, err := r.currentJob(ctx, obj, inputHash)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		if err := r.startBuild(ctx, obj, inputs, inputHash, contextURL); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: buildPollInterval}, nil
	}
	return r.observeJob(ctx, obj, job, inputHash)
}

// resolveInputs gathers everything the hash needs, using only the API server.
func (r *DockerBuildReconciler) resolveInputs(ctx context.Context, obj *ociv1alpha1.DockerBuild) (build.Inputs, string, error) {
	spec := obj.Spec

	if spec.Push == nil {
		return build.Inputs{}, "", terminal("spec.push is required: the built image is produced by a Job in another pod, which cannot write to the controller's loopback-only serving endpoint")
	}

	ns := spec.Context.Namespace
	if ns == "" {
		ns = obj.Namespace
	}
	art, err := source.FluxSource(ctx, r.Client, spec.Context.Kind, ns, spec.Context.Name)
	if err != nil {
		var nf *source.ErrNotFound
		if errors.As(err, &nf) {
			// Creating the source fixes this, not editing this object.
			return build.Inputs{}, "", pending("build context: %s", err)
		}
		return build.Inputs{}, "", fmt.Errorf("build context: %w", err)
	}

	// Secret identities, never values. status.inputHash is world-readable to anyone with get, and
	// a hash of a low-entropy secret is an oracle.
	ids := make([]string, 0, len(spec.Secrets))
	for _, s := range spec.Secrets {
		var secret corev1.Secret
		key := types.NamespacedName{Namespace: obj.Namespace, Name: s.SecretRef.Name}
		if err := r.Get(ctx, key, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return build.Inputs{}, "", pending("build secret %q does not exist yet", s.SecretRef.Name)
			}
			return build.Inputs{}, "", fmt.Errorf("reading build secret %q: %w", s.SecretRef.Name, err)
		}
		ids = append(ids, s.SecretRef.Name+"/"+secret.ResourceVersion)
	}

	args := make([]build.Arg, 0, len(spec.Args))
	for _, a := range spec.Args {
		args = append(args, build.Arg{Name: a.Name, Value: a.Value})
	}

	cacheMode := "Auto"
	if spec.Cache != nil && spec.Cache.Mode != "" {
		cacheMode = spec.Cache.Mode
	}

	return build.Inputs{
		BuilderDigest:    r.JobConfig.BuilderImage,
		FrontendDigest:   r.JobConfig.FrontendImage,
		ContextDigest:    art.Digest,
		ContextSubpath:   spec.Context.Subpath,
		Dockerfile:       spec.Dockerfile,
		Target:           spec.Target,
		Network:          spec.Network,
		CacheMode:        cacheMode,
		CacheRef:         cacheRefFor(obj),
		SourceDateEpoch:  r.JobConfig.SourceDateEpoch,
		Platforms:        spec.Platforms,
		Args:             args,
		SecretIdentities: ids,
	}, art.URL, nil
}

// currentJob returns the Job for these inputs, adopting one left behind by a previous leader.
func (r *DockerBuildReconciler) currentJob(ctx context.Context, obj *ociv1alpha1.DockerBuild, inputHash string) (*batchv1.Job, error) {
	var job batchv1.Job
	key := types.NamespacedName{Namespace: obj.Namespace, Name: jobName(obj, inputHash)}
	switch err := r.Get(ctx, key, &job); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the build job: %w", err)
	}
	return &job, nil
}

// startBuild validates the Dockerfile and creates the Job.
func (r *DockerBuildReconciler) startBuild(ctx context.Context, obj *ociv1alpha1.DockerBuild,
	inputs build.Inputs, inputHash, contextURL string) error {

	job := buildJob(obj, inputHash, contextURL, r.JobConfig)
	if err := ctrl.SetControllerReference(obj, job, r.Scheme()); err != nil {
		return fmt.Errorf("setting owner: %w", err)
	}

	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another leader got there first. Deterministic naming makes this harmless.
			return nil
		}
		return fmt.Errorf("creating the build job: %w", err)
	}

	obj.Status.BuildRef = &ociv1alpha1.LocalObjectReference{Name: job.Name}
	obj.Status.LastAttempt = &ociv1alpha1.BuildAttempt{
		InputHash: inputHash,
		StartedAt: ptrTime(metav1.Now()),
	}
	return nil
}

// observeJob turns a Job's state into this object's.
func (r *DockerBuildReconciler) observeJob(ctx context.Context, obj *ociv1alpha1.DockerBuild,
	job *batchv1.Job, inputHash string) (ctrl.Result, error) {

	switch {
	case jobSucceeded(job):
		digest, err := r.readResultDigest(ctx, obj, job)
		if err != nil {
			return ctrl.Result{}, err
		}
		r.recordSuccess(obj, inputHash, digest)
		return ctrl.Result{RequeueAfter: interval(obj)}, nil

	case jobFailed(job):
		// Deleted so the next attempt is a fresh Job rather than a permanently failed one that
		// would be adopted forever.
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
			!apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting the failed job: %w", err)
		}
		obj.Status.BuildRef = nil
		obj.Status.Failures++
		if obj.Status.LastAttempt != nil {
			obj.Status.LastAttempt.FinishedAt = ptrTime(metav1.Now())
			obj.Status.LastAttempt.Message = jobFailureMessage(job)
		}
		return ctrl.Result{}, fmt.Errorf("build failed: %s", jobFailureMessage(job))

	default:
		return ctrl.Result{RequeueAfter: buildPollInterval}, nil
	}
}

// buildMetadata is the shape buildctl --metadata-file writes.
type buildMetadata struct {
	Digest string `json:"containerimage.digest"`
}

// readResultDigest recovers the pushed digest from the Job's pod.
//
// The digest is the one thing that has to come back out of the build, and it cannot be derived:
// this is the point at which the output stops being a function of the spec and becomes an
// observation.
func (r *DockerBuildReconciler) readResultDigest(ctx context.Context, obj *ociv1alpha1.DockerBuild, job *batchv1.Job) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(obj.Namespace),
		client.MatchingLabels{"job-name": job.Name}); err != nil {
		return "", fmt.Errorf("listing build pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", pending("the build pod for %s has not been observed yet", job.Name)
	}

	// The metadata file lives in the pod's emptyDir, which the controller cannot read directly.
	// The build container echoes it as its termination message, which is the supported way to get
	// a small result out of a pod without granting exec.
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != "build" || cs.State.Terminated == nil {
				continue
			}
			raw := strings.TrimSpace(cs.State.Terminated.Message)
			if raw == "" {
				continue
			}
			var md buildMetadata
			if err := json.Unmarshal([]byte(raw), &md); err != nil {
				continue
			}
			if md.Digest != "" {
				if obj.Status.LastAttempt != nil {
					obj.Status.LastAttempt.PodName = p.Name
				}
				return md.Digest, nil
			}
		}
	}
	return "", fmt.Errorf("the build reported no image digest; check `kubectl logs job/%s`", job.Name)
}

// recordSuccess writes the artifact and rotates history.
func (r *DockerBuildReconciler) recordSuccess(obj *ociv1alpha1.DockerBuild, inputHash, digest string) {
	repo := obj.Spec.Push.Repository
	tags := make([]string, 0, len(obj.Spec.Push.Tags))
	for _, t := range obj.Spec.Push.Tags {
		tags = append(tags, repo+":"+t)
	}

	revision := digest
	if len(obj.Spec.Push.Tags) > 0 {
		revision = obj.Spec.Push.Tags[0] + "@" + digest
	}

	obj.Status.Artifact = &ociv1alpha1.ArtifactStatus{
		Digest:   digest,
		Revision: revision,
		Ref:      repo + "@" + digest,
		Tags:     tags,
	}
	obj.Status.InputHash = inputHash
	obj.Status.BuildRef = nil
	obj.Status.Failures = 0
	if obj.Status.LastAttempt != nil {
		obj.Status.LastAttempt.FinishedAt = ptrTime(metav1.Now())
		obj.Status.LastAttempt.Succeeded = true
	}

	// InputHash on the record, unlike the composer's, so a controller that lost status.artifact
	// but kept history can find what it previously produced for these inputs and re-verify rather
	// than rebuild blind — which is the mitigation ADR 0025 names for the permanent-conflict trap.
	record := ociv1alpha1.BuildRecord{Digest: digest, Tags: tags, InputHash: inputHash}
	history := append([]ociv1alpha1.BuildRecord{record}, obj.Status.History...)

	limit := r.HistoryLimit
	if limit <= 0 {
		limit = 10
	}
	if len(history) > limit {
		history = history[:limit]
	}
	obj.Status.History = history
}

// applyOutcome sets the conditions for whatever just happened.
func (r *DockerBuildReconciler) applyOutcome(obj *ociv1alpha1.DockerBuild, err error) {
	switch {
	case err == nil:
		setCondition(obj, ociv1alpha1.ReadyCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonSucceeded, readyMessage(obj))
		removeCondition(obj, ociv1alpha1.StalledCondition)
		removeCondition(obj, ociv1alpha1.ReconcilingCondition)

	case isTerminal(err):
		setCondition(obj, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
			ociv1alpha1.ReasonInvalidSpec, err.Error())
		setCondition(obj, ociv1alpha1.StalledCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonInvalidSpec, err.Error())
		removeCondition(obj, ociv1alpha1.ReconcilingCondition)

	case isPending(err):
		setCondition(obj, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
			ociv1alpha1.ReasonDependencyNotReady, err.Error())
		setCondition(obj, ociv1alpha1.ReconcilingCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonDependencyNotReady, err.Error())
		removeCondition(obj, ociv1alpha1.StalledCondition)

	default:
		// Never Stalled. The Dockerfile that would fix a failing RUN lives in another object, so
		// stalling would wait for a generation change that never comes.
		setCondition(obj, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
			ociv1alpha1.ReasonBuildFailed, err.Error())
		setCondition(obj, ociv1alpha1.ReconcilingCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonBuildFailed, err.Error())
		removeCondition(obj, ociv1alpha1.StalledCondition)
	}
}

func readyMessage(obj *ociv1alpha1.DockerBuild) string {
	if obj.Status.Artifact == nil {
		return "reconciled"
	}
	return "built " + obj.Status.Artifact.Ref
}

func setCondition(obj *ociv1alpha1.DockerBuild, condType string, status metav1.ConditionStatus, reason, message string) {
	if len(message) > 32768 {
		message = message[:32768]
	}
	meta := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: obj.Generation,
	}
	for i := range obj.Status.Conditions {
		if obj.Status.Conditions[i].Type == condType {
			if obj.Status.Conditions[i].Status != status {
				meta.LastTransitionTime = metav1.Now()
			} else {
				meta.LastTransitionTime = obj.Status.Conditions[i].LastTransitionTime
			}
			obj.Status.Conditions[i] = meta
			return
		}
	}
	meta.LastTransitionTime = metav1.Now()
	obj.Status.Conditions = append(obj.Status.Conditions, meta)
}

func removeCondition(obj *ociv1alpha1.DockerBuild, condType string) {
	out := obj.Status.Conditions[:0]
	for _, c := range obj.Status.Conditions {
		if c.Type != condType {
			out = append(out, c)
		}
	}
	obj.Status.Conditions = out
}

func interval(obj *ociv1alpha1.DockerBuild) time.Duration {
	if obj.Spec.Interval != nil && obj.Spec.Interval.Duration > 0 {
		return obj.Spec.Interval.Duration
	}
	return time.Hour
}

func jobSucceeded(job *batchv1.Job) bool { return job.Status.Succeeded > 0 }
func jobFailed(job *batchv1.Job) bool    { return job.Status.Failed > 0 }

func jobFailureMessage(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return c.Message
			}
			return c.Reason
		}
	}
	return "the build job failed"
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

func isTerminal(err error) bool {
	var t *terminalError
	return errors.As(err, &t)
}

func isPending(err error) bool {
	var p *pendingError
	return errors.As(err, &p)
}

// SetupWithManager wires the controller and its owned Jobs.
func (r *DockerBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ociv1alpha1.DockerBuild{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
