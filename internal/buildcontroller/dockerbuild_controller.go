package buildcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
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

	// Recorder surfaces failures as Events. A build failure's detail lives in the pod's logs,
	// which vanish with the pod, so the Event is often the only durable trace of why.
	Recorder record.EventRecorder

	// HTTPClient fetches the build context for the Dockerfile check. Nil uses a default with a
	// timeout; the build itself never streams through this process.
	HTTPClient *http.Client

	// HistoryLimit is how many past builds are retained in status.
	HistoryLimit int
}

// +kubebuilder:rbac:groups=oci.lhns.de,resources=dockerbuilds,verbs=get;list;watch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=dockerbuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
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
	// Suspended objects say so, rather than going quiet and looking stalled.
	if obj.Spec.Suspend {
		patch := client.MergeFrom(obj.DeepCopy())
		setCondition(&obj, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
			ociv1alpha1.ReasonSuspended, "Reconciliation is suspended")
		removeCondition(&obj, ociv1alpha1.ReconcilingCondition)
		return ctrl.Result{}, r.Status().Patch(ctx, &obj, patch)
	}

	patch := client.MergeFrom(obj.DeepCopy())
	result, err := r.reconcile(ctx, &obj)

	obj.Status.ObservedGeneration = obj.Generation
	// Echoed on every completed pass, failures included: a client waiting for the request to land
	// must not hang because the build it asked for failed (ADR 0009).
	obj.Status.LastHandledReconcileAt = obj.Annotations[ociv1alpha1.ReconcileRequestAnnotation]
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
		r.event(&obj, corev1.EventTypeWarning, ociv1alpha1.ReasonInvalidSpec, err.Error())
		return ctrl.Result{}, nil
	case isPending(err):
		return ctrl.Result{RequeueAfter: pendingRetryInterval}, nil
	default:
		// A build failure, or anything else transient. Capped backoff rather than exponential
		// forever, because the fix is usually a push to the Dockerfile's repository and the retry
		// is what notices it.
		logger.Error(err, "build failed", "failures", obj.Status.Failures)
		r.event(&obj, corev1.EventTypeWarning, ociv1alpha1.ReasonBuildFailed, err.Error())
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

	// Note what is NOT checked here — that the artifact is still present in the
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

	args := make(map[string]string, len(spec.Args))
	for _, a := range spec.Args {
		args[a.Name] = a.Value
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

	// The FROM check happens here rather than inside the Job, so an unpinned base is refused
	// BEFORE anything executes. That costs one fetch of the context — but only on the path where a
	// build is about to run anyway, never on the cheap reconcile that finds an unchanged hash.
	dockerfile, err := build.FetchDockerfile(ctx, r.httpClient(), contextURL,
		obj.Spec.Context.Subpath, obj.Spec.Dockerfile)
	if err != nil {
		return fmt.Errorf("reading the Dockerfile: %w", err)
	}
	if err := build.CheckPinnedBases(bytes.NewReader(dockerfile)); err != nil {
		// Not terminal: the Dockerfile lives in the Flux source, so pushing a fix there is what
		// resolves this, and that raises no generation change on this object.
		return fmt.Errorf("%w", err)
	}

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
		StartedAt: ptr.To(metav1.Now()),
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
		// The failed Job is KEPT until its backoff has elapsed, and deleted only when the next
		// attempt is actually due. Deleting it as soon as the failure is seen fires this
		// controller's own Job watch, which reconciles immediately, finds no Job and starts
		// another — so the RequeueAfter backoff never applies and a failing build retries in a hot
		// loop. Keeping it also keeps the pod, which is the only place the reason a build failed is
		// written down; deleting on sight destroys the evidence before anyone can read it.
		msg := storedFailureMessage(obj, job)
		switch {
		case obj.Status.BuildRef != nil:
			// First observation of this failure: count it once, and read the pod while it is still
			// there. Later passes reuse this rather than listing pods again on every backoff poll,
			// which would also let the message degrade once the pod is collected.
			msg = r.jobFailureDetail(ctx, obj, job)
			obj.Status.BuildRef = nil
			obj.Status.Failures++
			if obj.Status.LastAttempt != nil {
				obj.Status.LastAttempt.FinishedAt = ptr.To(metav1.Now())
				obj.Status.LastAttempt.Message = msg
			}
		case retryDue(obj):
			// Already counted, and the wait is over. The delete wakes this controller through the
			// Job watch, and that reconcile is the one that starts the fresh Job.
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
				!apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("deleting the failed job: %w", err)
			}
		}
		// Returned as an error on every pass, including while waiting: Ready must stay False, and
		// Reconcile's own backoff already spaces the retries.
		return ctrl.Result{}, fmt.Errorf("build failed: %s", msg)

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
// The digest is the one thing that has to come back out of the build, and it cannot be derived.
func (r *DockerBuildReconciler) readResultDigest(ctx context.Context, obj *ociv1alpha1.DockerBuild, job *batchv1.Job) (string, error) {
	pods, err := r.buildPods(ctx, obj, job)
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", pending("the build pod for %s has not been observed yet", job.Name)
	}

	// The metadata file lives in the pod's emptyDir, which the controller cannot read. The build
	// container copies it to the termination log, which Kubernetes surfaces here — the supported
	// way to get a small result out of a pod without granting exec.
	for _, p := range pods.Items {
		if digest := podBuildDigest(p); digest != "" {
			if obj.Status.LastAttempt != nil {
				obj.Status.LastAttempt.PodName = p.Name
			}
			return digest, nil
		}
	}
	return "", fmt.Errorf("the build reported no image digest; check `kubectl logs job/%s`", job.Name)
}

// event records one, if a recorder was wired. Nil in tests that do not care.
func (r *DockerBuildReconciler) event(obj *ociv1alpha1.DockerBuild, kind, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, kind, reason, truncate(msg, 1024))
	}
}

// podBuildDigest returns the digest the build container reported, or "" if it reported none.
func podBuildDigest(p corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != "build" || cs.State.Terminated == nil {
			continue
		}
		var md buildMetadata
		if err := json.Unmarshal([]byte(strings.TrimSpace(cs.State.Terminated.Message)), &md); err != nil {
			continue
		}
		if md.Digest != "" {
			return md.Digest
		}
	}
	return ""
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
		obj.Status.LastAttempt.FinishedAt = ptr.To(metav1.Now())
		obj.Status.LastAttempt.Succeeded = true
	}

	// InputHash on the record, unlike the composer's — see BuildRecord.InputHash.
	record := ociv1alpha1.BuildRecord{Digest: digest, Tags: tags, InputHash: inputHash}
	history := append([]ociv1alpha1.BuildRecord{record}, obj.Status.History...)

	limit := r.HistoryLimit
	if limit <= 0 {
		limit = ociv1alpha1.DefaultHistoryLimit
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
		// Never Stalled: the fix lives in another object. See errors.go.
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

func setCondition(obj *ociv1alpha1.DockerBuild, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            truncate(msg, 32768),
		ObservedGeneration: obj.Generation,
	})
}

func removeCondition(obj *ociv1alpha1.DockerBuild, condType string) {
	meta.RemoveStatusCondition(&obj.Status.Conditions, condType)
}

// truncate keeps a message inside the API server's per-condition limit.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func interval(obj *ociv1alpha1.DockerBuild) time.Duration {
	if obj.Spec.Interval != nil && obj.Spec.Interval.Duration > 0 {
		return obj.Spec.Interval.Duration
	}
	return time.Hour
}

// httpClient is the client used for the Dockerfile pre-check.
func (r *DockerBuildReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

func jobSucceeded(job *batchv1.Job) bool { return job.Status.Succeeded > 0 }
func jobFailed(job *batchv1.Job) bool    { return job.Status.Failed > 0 }

// buildPods lists the pods of one build's Job.
func (r *DockerBuildReconciler) buildPods(ctx context.Context, obj *ociv1alpha1.DockerBuild,
	job *batchv1.Job) (corev1.PodList, error) {

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(obj.Namespace),
		client.MatchingLabels{"job-name": job.Name}); err != nil {
		return pods, fmt.Errorf("listing build pods: %w", err)
	}
	return pods, nil
}

// storedFailureMessage is what a previous pass already worked out about this failure.
func storedFailureMessage(obj *ociv1alpha1.DockerBuild, job *batchv1.Job) string {
	if la := obj.Status.LastAttempt; la != nil && la.Message != "" {
		return la.Message
	}
	return jobFailureMessage(job)
}

// retryDue reports whether enough time has passed since the last failure to try again. It mirrors
// the interval Reconcile requeues at, so the wait is the backoff rather than a second policy.
func retryDue(obj *ociv1alpha1.DockerBuild) bool {
	la := obj.Status.LastAttempt
	if la == nil || la.FinishedAt == nil {
		return true
	}
	return !time.Now().Before(la.FinishedAt.Add(failureBackoff(obj.Status.Failures)))
}

// jobFailureDetail explains a failed build as specifically as the cluster allows.
//
// The Job's own condition says only "BackoffLimitExceeded", which names the mechanism and not the
// cause. The cause is the build container's exit code and termination message, so those are read
// from the pod and appended — otherwise status shows a failure with no way to act on it.
func (r *DockerBuildReconciler) jobFailureDetail(ctx context.Context, obj *ociv1alpha1.DockerBuild, job *batchv1.Job) string {
	msg := jobFailureMessage(job)

	pods, err := r.buildPods(ctx, obj, job)
	if err != nil {
		return msg
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			t := cs.State.Terminated
			if t == nil || t.ExitCode == 0 {
				continue
			}
			detail := fmt.Sprintf("%s: container %q exited %d", msg, cs.Name, t.ExitCode)
			if t.Reason != "" {
				detail += " (" + t.Reason + ")"
			}
			if m := strings.TrimSpace(t.Message); m != "" {
				detail += ": " + m
			}
			return detail + fmt.Sprintf("; see `kubectl -n %s logs %s -c %s`", p.Namespace, p.Name, cs.Name)
		}
	}
	return msg
}

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
