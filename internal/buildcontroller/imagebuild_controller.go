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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/build"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
	"github.com/lhns/kube-oci-composer/internal/source"
)

// ImageBuildReconciler runs a Job per build and records what it produced.
//
// The reconcile is deliberately the composer's three-phase shape: resolve everything from the API
// without transferring anything, hash it, and only past that point do expensive work. That is what
// answers ADR 0001's objection — "the reconcile loop would have to rebuild to discover whether a
// rebuild was needed" — because every input here is resolvable from the API server. What it cannot
// do is the composer's SECOND check, against the real output digest, because there is nothing to
// compare against until a build has run. See ADR 0025.
type ImageBuildReconciler struct {
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

// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagebuilds,verbs=get;list;watch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagebuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// Secrets are read for their resourceVersion only, so a rotation moves the input hash and
// rebuilds. The VALUE is never read here — it is projected straight into the build pod — which is
// why this is get and not list or watch, matching the composer's reasoning about blast radius.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories;ocirepositories;buckets,verbs=get;list;watch

func (r *ImageBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var obj ociv1alpha1.ImageBuild
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Suspended objects say so, rather than going quiet and looking stalled.
	if obj.Spec.Suspend {
		patch := client.MergeFrom(obj.DeepCopy())
		recon.SetCondition(&obj, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
			ociv1alpha1.ReasonSuspended, "Reconciliation is suspended")
		recon.RemoveCondition(&obj, ociv1alpha1.ReconcilingCondition)
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
	case recon.IsTerminal(err):
		// Stalled. No requeue: the generation change from editing the spec is the wake-up.
		logger.Error(err, "stalled")
		recon.Event(r.Recorder, &obj, corev1.EventTypeWarning, ociv1alpha1.ReasonInvalidSpec, err.Error())
		return ctrl.Result{}, nil
	case recon.IsPending(err):
		return ctrl.Result{RequeueAfter: pendingRetryInterval}, nil
	default:
		// A build failure, or anything else transient. Capped backoff rather than exponential
		// forever, because the fix is usually a push to the Dockerfile's repository and the retry
		// is what notices it.
		logger.Error(err, "build failed", "failures", obj.Status.Failures)
		recon.Event(r.Recorder, &obj, corev1.EventTypeWarning, ociv1alpha1.ReasonBuildFailed, err.Error())
		return ctrl.Result{RequeueAfter: failureBackoff(obj.Status.Failures)}, nil
	}
}

// reconcile is the state machine over the owned Job.
func (r *ImageBuildReconciler) reconcile(ctx context.Context, obj *ociv1alpha1.ImageBuild) (ctrl.Result, error) {
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
		return ctrl.Result{RequeueAfter: recon.Interval(obj.Spec.Interval)}, nil
	}

	// Adopt, observe or start.
	job, err := r.currentJob(ctx, obj, inputHash)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		// Before anything executes. A Job that has started cannot be un-pushed, so a conflict
		// noticed afterwards is a conflict that has already happened.
		stop, conflict, err := r.checkTagConflict(ctx, obj)
		if err != nil {
			return ctrl.Result{}, err
		}
		if stop {
			r.recordKept(obj, conflict)
			return ctrl.Result{RequeueAfter: recon.Interval(obj.Spec.Interval)}, nil
		}
		if err := r.startBuild(ctx, obj, inputs, inputHash, contextURL); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: buildPollInterval}, nil
	}
	return r.observeJob(ctx, obj, job, inputs, inputHash)
}

// resolveInputs gathers everything the hash needs, using only the API server.
func (r *ImageBuildReconciler) resolveInputs(ctx context.Context, obj *ociv1alpha1.ImageBuild) (build.Inputs, string, error) {
	spec := obj.Spec

	if spec.Push == nil {
		return build.Inputs{}, "", recon.Terminal("spec.push is required: the built image is produced by a Job in another pod, which cannot write to the controller's loopback-only serving endpoint")
	}

	// Validated before a Job exists: a malformed ref must not reach buildctl as a broken push
	// target, and editing this spec is what fixes it.
	if _, err := recon.EffectiveTags(spec.Push.GetTags(), spec.Push.GetRef()); err != nil {
		return build.Inputs{}, "", err
	}

	// Same namespace only, for the reason the composer refuses it: the RBAC is cluster-wide, so
	// naming another namespace's source would let anyone who can create an ImageBuild read content
	// they have no access to.
	ns := obj.Namespace
	if spec.Context.Namespace != "" && spec.Context.Namespace != obj.Namespace {
		return build.Inputs{}, "", recon.Terminal(
			"build context %s/%s is in namespace %q: a context must be in the same namespace as the "+
				"ImageBuild that consumes it", spec.Context.Kind, spec.Context.Name, spec.Context.Namespace)
	}
	art, err := source.FluxSource(ctx, r.Client, spec.Context.Kind, ns, spec.Context.Name)
	if err != nil {
		var nf *source.ErrNotFound
		if errors.As(err, &nf) {
			// Creating the source fixes this, not editing this object.
			return build.Inputs{}, "", recon.Pending("build context: %s", err)
		}
		return build.Inputs{}, "", fmt.Errorf("build context: %w", err)
	}

	// Same rule as the composer's layers: an explicit revision waits for the source to reach it
	// rather than building from whatever is currently published.
	if !ociv1alpha1.RevisionMatches(spec.Context.Revision, art.Revision) {
		return build.Inputs{}, "", recon.Pending(
			"build context %s/%s is at revision %q, waiting for %q",
			spec.Context.Kind, spec.Context.Name, art.Revision, spec.Context.Revision)
	}

	// Secret identities, never values. status.inputHash is world-readable to anyone with get, and
	// a hash of a low-entropy secret is an oracle.
	ids := make([]string, 0, len(spec.Secrets))
	for _, s := range spec.Secrets {
		var secret corev1.Secret
		key := types.NamespacedName{Namespace: obj.Namespace, Name: s.SecretRef.Name}
		if err := r.Get(ctx, key, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return build.Inputs{}, "", recon.Pending("build secret %q does not exist yet", s.SecretRef.Name)
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
		ContextRevision:  art.Revision,
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
func (r *ImageBuildReconciler) currentJob(ctx context.Context, obj *ociv1alpha1.ImageBuild, inputHash string) (*batchv1.Job, error) {
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
func (r *ImageBuildReconciler) startBuild(ctx context.Context, obj *ociv1alpha1.ImageBuild,
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
func (r *ImageBuildReconciler) observeJob(ctx context.Context, obj *ociv1alpha1.ImageBuild,
	job *batchv1.Job, inputs build.Inputs, inputHash string) (ctrl.Result, error) {

	switch {
	case jobSucceeded(job):
		digest, err := r.readResultDigest(ctx, obj, job)
		if err != nil {
			return ctrl.Result{}, err
		}
		r.recordSuccess(obj, inputs, inputHash, digest)
		return ctrl.Result{RequeueAfter: recon.Interval(obj.Spec.Interval)}, nil

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
func (r *ImageBuildReconciler) readResultDigest(ctx context.Context, obj *ociv1alpha1.ImageBuild, job *batchv1.Job) (string, error) {
	pods, err := r.buildPods(ctx, obj, job)
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", recon.Pending("the build pod for %s has not been observed yet", job.Name)
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
func (r *ImageBuildReconciler) recordSuccess(obj *ociv1alpha1.ImageBuild, inputs build.Inputs, inputHash, digest string) {
	repo := obj.Spec.Push.Repository
	// Same list the Job was told to push, so status cannot describe a different set of tags than
	// the build actually wrote.
	effective, err := recon.EffectiveTags(obj.Spec.Push.GetTags(), obj.Spec.Push.GetRef())
	if err != nil {
		effective = obj.Spec.Push.Tags
	}
	tags := make([]string, 0, len(effective))
	for _, t := range effective {
		tags = append(tags, repo+":"+t)
	}

	revision := digest
	if len(effective) > 0 {
		revision = effective[0] + "@" + digest
	}

	obj.Status.Artifact = &ociv1alpha1.ArtifactStatus{
		Digest:   digest,
		Revision: revision,
		Ref:      repo + "@" + digest,
		Tags:     tags,
	}
	obj.Status.InputHash = inputHash
	// A build that published is a divergence resolved, so the record must go. Leaving it would make
	// the field a permanent scar on an object that is now correct.
	obj.Status.Conflict = nil
	obj.Status.BuildRef = nil
	obj.Status.Failures = 0
	if obj.Status.LastAttempt != nil {
		obj.Status.LastAttempt.FinishedAt = ptr.To(metav1.Now())
		obj.Status.LastAttempt.Succeeded = true
	}

	// InputHash on the record, unlike the composer's — see BuildRecord.InputHash.
	record := ociv1alpha1.BuildRecord{
		Digest: digest, Tags: tags, InputHash: inputHash,
		// Where the build came from, so an artifact can be traced back to a revision without
		// pulling it apart. The composer records this per layer; a build has exactly one source.
		Sources: []ociv1alpha1.SourceRecord{{
			Name:     obj.Spec.Context.Name,
			Revision: inputs.ContextRevision,
			Digest:   inputs.ContextDigest,
		}},
	}
	limit := r.historyLimit(obj)
	obj.Status.History = recon.RecordHistory(obj.Status.History, &record, limit)
}

// historyLimit is the object's own retention if it sets one, else the operator's, else the default.
//
// Per-object retention matters MORE here than on a composition: a composition can rebuild any
// artifact from its spec, so retention is a convenience. A build cannot (ADR 0025), so this is how
// much of the only copy is kept.
func (r *ImageBuildReconciler) historyLimit(obj *ociv1alpha1.ImageBuild) int {
	if p := obj.Spec.Push; p != nil && p.History != nil && *p.History > 0 {
		return int(*p.History)
	}
	if r.HistoryLimit > 0 {
		return r.HistoryLimit
	}
	return ociv1alpha1.DefaultHistoryLimit
}

// applyOutcome sets the conditions for whatever just happened.
func (r *ImageBuildReconciler) applyOutcome(obj *ociv1alpha1.ImageBuild, err error) {
	switch {
	case err == nil:
		recon.SetCondition(obj, ociv1alpha1.ReadyCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonSucceeded, readyMessage(obj))
		recon.RemoveCondition(obj, ociv1alpha1.StalledCondition)
		recon.RemoveCondition(obj, ociv1alpha1.ReconcilingCondition)

	case recon.IsTerminal(err):
		recon.SetCondition(obj, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
			ociv1alpha1.ReasonInvalidSpec, err.Error())
		recon.SetCondition(obj, ociv1alpha1.StalledCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonInvalidSpec, err.Error())
		recon.RemoveCondition(obj, ociv1alpha1.ReconcilingCondition)

	case recon.IsPending(err):
		recon.SetCondition(obj, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
			ociv1alpha1.ReasonDependencyNotReady, err.Error())
		recon.SetCondition(obj, ociv1alpha1.ReconcilingCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonDependencyNotReady, err.Error())
		recon.RemoveCondition(obj, ociv1alpha1.StalledCondition)

	default:
		// Never Stalled: the fix lives in another object. See errors.go.
		recon.SetCondition(obj, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
			ociv1alpha1.ReasonBuildFailed, err.Error())
		recon.SetCondition(obj, ociv1alpha1.ReconcilingCondition, metav1.ConditionTrue,
			ociv1alpha1.ReasonBuildFailed, err.Error())
		recon.RemoveCondition(obj, ociv1alpha1.StalledCondition)
	}
}

func readyMessage(obj *ociv1alpha1.ImageBuild) string {
	// A kept tag comes first: it is the case where the object is Ready and yet did NOT do what its
	// spec asks for, so an operator reading one line has to see it here rather than go looking.
	if c := obj.Status.Conflict; c != nil {
		return fmt.Sprintf("kept %s at %s; no build was run (onConflict: Keep)", c.Tag, c.Existing)
	}
	if obj.Status.Artifact == nil {
		return "reconciled"
	}
	return "built " + obj.Status.Artifact.Ref
}

// httpClient is the client used for the Dockerfile pre-check.
func (r *ImageBuildReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

func jobSucceeded(job *batchv1.Job) bool { return job.Status.Succeeded > 0 }
func jobFailed(job *batchv1.Job) bool    { return job.Status.Failed > 0 }

// buildPods lists the pods of one build's Job.
func (r *ImageBuildReconciler) buildPods(ctx context.Context, obj *ociv1alpha1.ImageBuild,
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
func storedFailureMessage(obj *ociv1alpha1.ImageBuild, job *batchv1.Job) string {
	if la := obj.Status.LastAttempt; la != nil && la.Message != "" {
		return la.Message
	}
	return jobFailureMessage(job)
}

// retryDue reports whether enough time has passed since the last failure to try again. It mirrors
// the interval Reconcile requeues at, so the wait is the backoff rather than a second policy.
func retryDue(obj *ociv1alpha1.ImageBuild) bool {
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
func (r *ImageBuildReconciler) jobFailureDetail(ctx context.Context, obj *ociv1alpha1.ImageBuild, job *batchv1.Job) string {
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

// SetupWithManager wires the controller and its owned Jobs.
func (r *ImageBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ociv1alpha1.ImageBuild{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

// recordKept notes a build that was not run because onConflict: Keep left an existing tag alone.
//
// Conditions are deliberately NOT set here. applyOutcome owns every condition on this kind, and
// readyMessage renders this case; setting them in both places would make the message depend on call
// order, which is how the Ready condition ends up disagreeing with itself.
//
// The outcome is Ready, not Stalled. With a spec-hash tag an existing tag means these inputs have
// already been built and published, so nothing is wrong -- and stalling would need a generation
// change to recover from, which changing the upstream Dockerfile does not produce.
//
// status.artifact is deliberately NOT synthesised from what the tag holds. On this kind the digest
// alone does not say which inputs produced it, so claiming it as this object's artifact would
// assert something unverifiable. status.conflict says exactly what is known: the tag exists, and
// this is what it points at.
func (r *ImageBuildReconciler) recordKept(obj *ociv1alpha1.ImageBuild, c *ociv1alpha1.TagConflictStatus) {
	obj.Status.Conflict = c
	obj.Status.Failures = 0
	recon.Event(r.Recorder, obj, corev1.EventTypeNormal, ociv1alpha1.ReasonSucceeded,
		fmt.Sprintf("Kept %s at %s; no build was run (onConflict: Keep)", c.Tag, c.Existing))
}
