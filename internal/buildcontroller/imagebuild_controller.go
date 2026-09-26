package buildcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/attest"
	"github.com/lhns/kube-oci-composer/internal/build"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
	"github.com/lhns/kube-oci-composer/internal/retention"
	"github.com/lhns/kube-oci-composer/internal/source"
)

// ImageBuildReconciler runs a Job per build and records what it produced.
//
// Like the composer, it resolves every input from the API server and hashes it before doing any
// expensive work. Unlike the composer it cannot verify against the output digest, since there is
// none until a build has run. ADR 0025.
type ImageBuildReconciler struct {
	client.Client
	JobConfig JobConfig

	// Default is the operator's registry and credential, used by builds that name no repository of
	// their own. See recon.DefaultRegistry.
	Default recon.DefaultRegistry

	// Recorder surfaces failures as Events, often the only durable trace once the pod is gone.
	Recorder record.EventRecorder

	// Refresher renews an artifact's lease the moment it is published. Nil disables it
	// (--retention-refresh-interval=0).
	Refresher *retention.Refresher
	// Export is the operator's policy for push.writeRefTo: permitted foreign namespaces and
	// metadata keys. The controller is the boundary here, not RBAC (ADR 0056).
	Export recon.ExportOptions

	// BuildPollInterval is how often a running Job is re-observed, and so how long a pushed but
	// untagged manifest is exposed to collection. Zero means defaultBuildPollInterval.
	BuildPollInterval time.Duration

	// Attestor signs the build's output after the Job has terminated. The key stays in this
	// process and is never projected into a build pod; SBOM and provenance come from BuildKit.
	Attestor *attest.Attestor

	// Transport, when set, trusts an additional CA on top of the system roots. See recon.Transport.
	Transport http.RoundTripper

	// HTTPClient fetches the build context for the Dockerfile check. Nil uses http.DefaultClient.
	HTTPClient *http.Client

	// HistoryLimit is how many past builds are retained in status.
	HistoryLimit int

	// RequirePinnedSources refuses a spec.context that names no revision (threat T1). A build's
	// output cannot be reproduced from its spec (ADR 0025), so an unpinned context is unauditable.
	RequirePinnedSources bool
}

// patch: the export finalizer is added and removed by patching the object itself; the finalizers
// subresource grant below does not cover that.
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagebuilds,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagebuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagebuilds/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// Secrets: get reads only the resourceVersion (for the input hash); values are projected straight
// into the build pod. create/update are for per-build copies of the operator's credentials in the
// build's namespace, since a pod mounts Secrets only from its own. `create` cannot be restricted by
// name, but without list/watch the controller only touches Secrets whose names it knows.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update
//
// ConfigMaps are watched (unlike Secrets) so an edited Dockerfile rebuilds promptly. The write
// verbs are push.writeRefTo's; cluster-wide because the controller, not RBAC, enforces the allowed
// namespaces and deletes only what carries its labels. ADR 0056. No deletecollection.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories;ocirepositories;buckets,verbs=get;list;watch

func (r *ImageBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var obj ociv1alpha1.ImageBuild
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The patch base, taken before anything below edits status in memory.
	pristine := obj.DeepCopy()

	// Before suspend and before anything that can stall: it names content status says this object
	// already published, and that must happen whether or not the spec reconciles (ADR 0060). Its
	// status changes ride on whichever patch this pass ends with.
	if obj.DeletionTimestamp.IsZero() {
		r.backfillDigestTags(ctx, &obj)
	}
	// Not while deleting: the finalizer is only removed below, so returning here would leave the
	// object Terminating forever. observedGeneration must advance too, or the retention refresher
	// sees this object as pending and skips its whole cycle for every image in the cluster.
	if obj.Spec.Suspend && obj.DeletionTimestamp.IsZero() {
		patch := client.MergeFrom(pristine)
		recon.SetCondition(&obj, ociv1alpha1.ReadyCondition, metav1.ConditionFalse,
			ociv1alpha1.ReasonSuspended, "Reconciliation is suspended")
		recon.RemoveCondition(&obj, ociv1alpha1.ReconcilingCondition)
		obj.Status.ObservedGeneration = obj.Generation
		return ctrl.Result{}, client.IgnoreNotFound(r.Status().Patch(ctx, &obj, patch))
	}

	patch := client.MergeFrom(pristine)
	result, err := r.reconcile(ctx, &obj)

	obj.Status.ObservedGeneration = obj.Generation
	// Echoed on every completed pass, failures included: a client waiting for the request to land
	// must not hang because the build it asked for failed (ADR 0009).
	obj.Status.LastHandledReconcileAt = obj.Annotations[ociv1alpha1.ReconcileRequestAnnotation]
	r.applyOutcome(&obj, err)
	// IgnoreNotFound: removing the last finalizer may already have deleted the object.
	if perr := client.IgnoreNotFound(r.Status().Patch(ctx, &obj, patch)); perr != nil {
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
		return ctrl.Result{RequeueAfter: recon.PendingRetryInterval}, nil
	default:
		// Transient, including build failures. Capped backoff: the fix is usually an upstream push
		// that only a retry will notice.
		logger.Error(err, "build failed", "failures", obj.Status.Failures)
		recon.Event(r.Recorder, &obj, corev1.EventTypeWarning, ociv1alpha1.ReasonBuildFailed, err.Error())
		return ctrl.Result{RequeueAfter: failureBackoff(obj.Status.Failures)}, nil
	}
}

// reconcile is the state machine over the owned Job.
func (r *ImageBuildReconciler) reconcile(ctx context.Context, obj *ociv1alpha1.ImageBuild) (ctrl.Result, error) {
	// status.RefExport too: removing writeRefTo must still clean up what it wrote.
	if exp := obj.Spec.Push.GetWriteRefTo(); !obj.DeletionTimestamp.IsZero() || exp != nil ||
		obj.Status.RefExport != nil {
		res, done, err := r.reconcileExportLifecycle(ctx, obj, exp)
		if done || err != nil {
			return res, err
		}
	}

	inputs, contextURL, err := r.resolveInputs(ctx, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	inputHash := inputs.Hash()

	// Unchanged inputs are not enough: the published image must still exist. A rebuild yields a
	// different digest (builds are not reproducible), hence the warning Event. ADR 0051.
	if obj.Status.Artifact != nil && obj.Status.InputHash == inputHash {
		if r.stillPublished(ctx, obj) {
			// writeRefTo is not in the input hash, so a converged object must export here or a
			// newly added, moved or deleted export would never be (re)written.
			if err := r.exportRef(ctx, obj); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: recon.Interval(obj.Spec.Interval)}, nil
		}
		recon.Event(r.Recorder, obj, corev1.EventTypeWarning, ociv1alpha1.ReasonArtifactLost,
			fmt.Sprintf("%s is no longer in the registry; rebuilding. The new image will have a "+
				"DIFFERENT digest, so anything referencing the old one by digest is not restored "+
				"by this.", obj.Status.Artifact.Digest))
	}

	// Adopt, observe or start.
	job, err := r.currentJob(ctx, obj, inputHash)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		// Before anything executes: a started Job cannot be un-pushed.
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
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}
	return r.observeJob(ctx, obj, job, inputs, inputHash)
}

// resolveInputs gathers everything the hash needs, using only the API server.
func (r *ImageBuildReconciler) resolveInputs(ctx context.Context, obj *ociv1alpha1.ImageBuild) (build.Inputs, string, error) {
	spec := obj.Spec

	if spec.Push == nil {
		return build.Inputs{}, "", recon.Terminal("spec.push is required: the built image is produced by a Job in another pod, which cannot write to the controller's loopback-only serving endpoint")
	}

	// Validated before a Job exists so a malformed ref never reaches buildctl.
	if _, err := recon.EffectiveTags(spec.Push.GetTags(), spec.Push.GetRef()); err != nil {
		return build.Inputs{}, "", err
	}

	resolved, err := r.resolveContext(ctx, obj)
	if err != nil {
		return build.Inputs{}, "", err
	}

	// Secret identities, never values: status.inputHash is readable by anyone with get, and a hash
	// of a low-entropy secret is an oracle.
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

	dockerfile, err := r.resolveDockerfile(ctx, obj)
	if err != nil {
		return build.Inputs{}, "", err
	}

	return build.Inputs{
		BuilderDigest:    r.JobConfig.BuilderImage,
		FrontendDigest:   r.JobConfig.FrontendImage,
		FetcherDigest:    r.JobConfig.FetcherImage,
		ContextKind:      resolved.kind,
		ContextDigest:    resolved.art.Digest,
		ContextRevision:  resolved.art.Revision,
		Attestations:     attestationMode(r.JobConfig),
		ContextSubpath:   resolved.subpath,
		ContextStrip:     resolved.strip,
		ContextUnpack:    resolved.unpack,
		DockerfileKind:   dockerfile.kind,
		Dockerfile:       dockerfile.path,
		DockerfileDigest: dockerfile.digest,
		Target:           spec.Target,
		Network:          spec.Network,
		CacheMode:        cacheMode,
		CacheRef:         cacheRefFor(obj, r.repositoryFor(obj)),
		SourceDateEpoch:  r.JobConfig.SourceDateEpoch,
		Platforms:        spec.Platforms,
		Args:             args,
		SecretIdentities: ids,
	}, resolved.art.URL, nil
}

// contextInputs is what a resolved context contributes to the input hash.
type contextInputs struct {
	kind    string
	subpath string
	strip   int
	unpack  string
	art     source.FluxArtifact
}

// resolveContext resolves whichever member spec.context names. No context is a legal empty tree;
// CEL refuses no context combined with a Dockerfile path.
func (r *ImageBuildReconciler) resolveContext(ctx context.Context, obj *ociv1alpha1.ImageBuild) (contextInputs, error) {
	switch c := obj.Spec.Context; {
	case c.GetImage() != nil:
		img := c.GetImage()
		// Hash only the digest: the tag is decorative, and a retag must not rebuild.
		_, digest, _ := strings.Cut(img.Ref, "@")
		return contextInputs{kind: "image", subpath: img.Subpath,
			art: source.FluxArtifact{URL: img.Ref, Digest: digest}}, nil

	case c.GetFetch() != nil:
		f := c.GetFetch()
		// The digest is declared; the build pod verifies the bytes against it (internal/fetchcontext).
		return contextInputs{kind: "fetch", subpath: f.Subpath, strip: f.StripComponents,
			unpack: string(f.Unpack),
			art:    source.FluxArtifact{URL: f.URL, Digest: f.Digest}}, nil

	case c.GetSourceRef() != nil:
		return r.resolveSourceRef(ctx, obj, c.GetSourceRef())

	default:
		return contextInputs{}, nil
	}
}

func (r *ImageBuildReconciler) resolveSourceRef(ctx context.Context, obj *ociv1alpha1.ImageBuild,
	ref *ociv1alpha1.SourceRefSource) (contextInputs, error) {
	// Same namespace only: RBAC is cluster-wide, so a foreign source would leak its content.
	if ref.Namespace != "" && ref.Namespace != obj.Namespace {
		return contextInputs{}, recon.Terminal(
			"build context %s/%s is in namespace %q: a context must be in the same namespace as "+
				"the ImageBuild that consumes it", ref.Kind, ref.Name, ref.Namespace)
	}
	// Threat T1. Terminal: editing this spec is the fix.
	if r.RequirePinnedSources && ref.Revision == "" {
		return contextInputs{}, recon.Terminal(
			"build context %s/%s names no revision, and this controller runs with "+
				"--require-pinned-sources: add `revision:` to pin the code this build runs",
			ref.Kind, ref.Name)
	}

	art, err := source.FluxSource(ctx, r.Client, ref.Kind, obj.Namespace, ref.Name)
	if err != nil {
		var nf *source.ErrNotFound
		if errors.As(err, &nf) {
			// Creating the source fixes this, not editing this object.
			return contextInputs{}, recon.Pending("build context: %s", err)
		}
		return contextInputs{}, fmt.Errorf("build context: %w", err)
	}

	// An explicit revision waits for the source to reach it.
	if !ociv1alpha1.RevisionMatches(ref.Revision, art.Revision) {
		return contextInputs{}, recon.Pending(
			"build context %s/%s is at revision %q, waiting for %q",
			ref.Kind, ref.Name, art.Revision, ref.Revision)
	}
	return contextInputs{kind: "sourceRef", subpath: ref.Subpath, art: art}, nil
}

// dockerfileInputs is the Dockerfile's identity, which depends on where it comes from.
type dockerfileInputs struct {
	kind   string
	path   string
	digest string
}

// resolveDockerfile decides what identifies the Dockerfile. A path is covered by ContextDigest;
// inline and ConfigMap content is hashed directly.
func (r *ImageBuildReconciler) resolveDockerfile(ctx context.Context, obj *ociv1alpha1.ImageBuild) (dockerfileInputs, error) {
	df := obj.Spec.Dockerfile

	switch {
	case df != nil && df.Inline != "":
		return dockerfileInputs{kind: "inline", digest: sha256Hex([]byte(df.Inline))}, nil

	case df != nil && df.ConfigMapRef != nil:
		// A ConfigMap is mutable, so it can never satisfy the pinning flag; refuse loudly.
		if r.RequirePinnedSources {
			return dockerfileInputs{}, recon.Terminal(
				"spec.dockerfile.configMapRef cannot be pinned and this controller runs with " +
					"--require-pinned-sources: put the Dockerfile in the context or in " +
					"spec.dockerfile.inline, where the spec itself pins it")
		}
		content, err := r.dockerfileFromConfigMap(ctx, obj)
		if err != nil {
			return dockerfileInputs{}, err
		}
		return dockerfileInputs{kind: "configMap", digest: sha256Hex(content)}, nil

	default:
		return dockerfileInputs{kind: "path", path: df.EffectiveDockerfile()}, nil
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
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

// dockerfileBytes returns the Dockerfile this build will run, and whether it is inline in the spec
// (which decides whether a bad FROM is terminal). Nil content means an image context, which the
// fetcher checks instead.
func (r *ImageBuildReconciler) dockerfileBytes(ctx context.Context, obj *ociv1alpha1.ImageBuild,
	contextURL string) (content []byte, inline bool, err error) {

	if df := obj.Spec.Dockerfile; df != nil && df.Inline != "" {
		return []byte(df.Inline), true, nil
	}
	if df := obj.Spec.Dockerfile; df != nil && df.ConfigMapRef != nil {
		// Re-read: these bytes are what the Job gets; an edit since resolveInputs moves the hash
		// next pass.
		content, err := r.dockerfileFromConfigMap(ctx, obj)
		return content, false, err
	}

	// Checked by the fetcher instead: reading an image here would need registry credentials for
	// arbitrary repositories in a shared controller. See internal/fetchcontext.
	if obj.Spec.Context.GetImage() != nil {
		return nil, false, nil
	}

	var subpath string
	var strip int
	if ref := obj.Spec.Context.GetSourceRef(); ref != nil {
		subpath = ref.Subpath
	}
	if f := obj.Spec.Context.GetFetch(); f != nil {
		// Only a fetch strips; a sourceRef never does (ADR 0045).
		subpath, strip = f.Subpath, f.StripComponents
	}
	content, err = build.FetchDockerfile(ctx, r.httpClient(), contextURL,
		subpath, obj.Spec.Dockerfile.EffectiveDockerfile(), strip)
	if err != nil {
		return nil, false, fmt.Errorf("reading the Dockerfile: %w", err)
	}
	return content, false, nil
}

// startBuild validates the Dockerfile and creates the Job.
func (r *ImageBuildReconciler) startBuild(ctx context.Context, obj *ociv1alpha1.ImageBuild,
	inputs build.Inputs, inputHash, contextURL string) error {

	// Check FROM lines before anything executes.
	dockerfile, inline, err := r.dockerfileBytes(ctx, obj, contextURL)
	if err != nil {
		return err
	}
	if dockerfile != nil {
		if err := build.CheckPinnedBases(bytes.NewReader(dockerfile)); err != nil {
			if inline {
				// Terminal only when inline: only then does the fix raise a generation change.
				return recon.Terminal("%s", err)
			}
			return err
		}
	}

	buildName := jobName(obj, inputHash)

	// Credentials must exist before the pod that mounts them.
	pushSecret, err := r.pushSecretFor(ctx, obj, buildName)
	if err != nil {
		return err
	}
	caSecret, err := r.registryCASecretFor(ctx, obj, buildName)
	if err != nil {
		return err
	}
	// Projects exactly the bytes checked above. Keyed off projectedDockerfile, the predicate the
	// Job rendering uses, not off `inline`, so the mount and its --local always agree.
	var dockerfileSecret string
	if projectedDockerfile(obj) {
		dockerfileSecret, err = r.dockerfileSecretFor(ctx, obj, buildName, dockerfile)
		if err != nil {
			return err
		}
	}

	// Only a sourceRef context is proxied and needs a token; fetch and image are pulled directly.
	// ADR 0044.
	var contextSecret string
	if obj.Spec.Context.GetSourceRef() != nil && r.JobConfig.ContextBaseURL != "" {
		contextSecret, err = r.contextTokenFor(ctx, obj, buildName)
		if err != nil {
			return err
		}
	}

	job := buildJob(obj, inputHash, contextURL, inputs.ContextDigest, r.JobConfig, r.repositoryFor(obj), pushSecret,
		caSecret, dockerfileSecret, contextSecret, r.cacheAvailable(ctx, obj))
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

	// Hand the Secrets to the Job so they go with it. ADR 0050.
	r.adoptBuildSecrets(ctx, obj, job)

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
		// The Job pushed by digest; tagging here is where onConflict is enforced. ADR 0054.
		conflict, err := r.applyTags(ctx, obj, digest)
		if err != nil {
			return ctrl.Result{}, err
		}
		if conflict != nil {
			// onConflict: Keep left the tag alone, so there is no artifact to record.
			r.recordKept(obj, conflict)
			return ctrl.Result{RequeueAfter: recon.Interval(obj.Spec.Interval)}, nil
		}

		r.recordSuccess(obj, inputs, inputHash, digest)
		// After recordSuccess, so status already names what was built when signing looks at it.
		obj.Status.Attestations = r.signBuild(ctx, obj, digest)
		r.refreshNow(ctx, obj)
		if err := r.exportRef(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: recon.Interval(obj.Spec.Interval)}, nil

	case jobFailed(job):
		// Keep the failed Job (and its pod's logs) until the backoff has elapsed. Deleting it
		// immediately fires the Job watch, which starts a new Job at once: a hot loop.
		msg := storedFailureMessage(obj, job)
		switch {
		case obj.Status.BuildRef != nil:
			// First observation: count it once and read the pod while it exists.
			msg = r.jobFailureDetail(ctx, obj, job)
			obj.Status.BuildRef = nil
			obj.Status.Failures++
			if obj.Status.LastAttempt != nil {
				obj.Status.LastAttempt.FinishedAt = ptr.To(metav1.Now())
				obj.Status.LastAttempt.Message = msg
			}
		case retryDue(obj):
			// The delete wakes the Job watch, whose reconcile starts the fresh Job.
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
				!apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("deleting the failed job: %w", err)
			}
		}
		// An error on every pass keeps Ready False; Reconcile's backoff spaces the retries.
		return ctrl.Result{}, fmt.Errorf("build failed: %s", msg)

	default:
		// Still running. Recorded even when adopted, so this object reports it as in flight.
		obj.Status.BuildRef = &ociv1alpha1.LocalObjectReference{Name: job.Name}
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}
}

// buildMetadata is the shape buildctl --metadata-file writes.
type buildMetadata struct {
	Digest string `json:"containerimage.digest"`
}

// readResultDigest recovers the pushed digest from the Job's pod.
func (r *ImageBuildReconciler) readResultDigest(ctx context.Context, obj *ociv1alpha1.ImageBuild, job *batchv1.Job) (string, error) {
	pods, err := r.buildPods(ctx, obj, job)
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", recon.Pending("the build pod for %s has not been observed yet", job.Name)
	}

	// The build container copies buildctl's metadata file to its termination message, which is
	// readable without exec.
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
	// Status gets the public pull name; anything that dials the registry uses repositoryFor
	// instead. ADR 0048.
	repo := r.Default.PublicRepository(r.repositoryFor(obj))
	// The same tag list the build was given.
	effective, err := recon.EffectiveTags(obj.Spec.Push.GetTags(), obj.Spec.Push.GetRef())
	if err != nil {
		effective = obj.Spec.Push.Tags
	}
	// Plus the digest's own tag, which applyTags adds to every accepted publish (ADR 0060).
	published := recon.PublishTags(effective, digest)
	tags := make([]string, 0, len(published))
	for _, t := range published {
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
	}
	// Traceability to a source revision; omitted for builds without a sourceRef context.
	if ref := obj.Spec.Context.GetSourceRef(); ref != nil {
		record.Sources = []ociv1alpha1.SourceRecord{{
			Name:     ref.Name,
			Revision: inputs.ContextRevision,
			Digest:   inputs.ContextDigest,
		}}
	}
	limit := r.historyLimit(obj)
	obj.Status.History = recon.RecordHistory(obj.Status.History, &record, limit)
}

// historyLimit is the object's own retention if it sets one, else the operator's, else the default.
func (r *ImageBuildReconciler) historyLimit(obj *ociv1alpha1.ImageBuild) int {
	return obj.Spec.Push.HistoryLimit(r.HistoryLimit)
}

// applyOutcome sets the conditions for whatever just happened.
func (r *ImageBuildReconciler) applyOutcome(obj *ociv1alpha1.ImageBuild, err error) {
	switch {
	case err == nil && obj.Status.BuildRef != nil:
		// A build is running. Ready would name the previous image to anything waiting (ADR 0061).
		published := ""
		if obj.Status.Artifact != nil {
			published = obj.Status.Artifact.Ref
		}
		recon.SetProgressing(obj, "building "+obj.Status.BuildRef.Name, published)

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
	// A kept tag first: Ready, yet the spec was not carried out.
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

// retryDue reports whether the failure backoff Reconcile requeues with has elapsed.
func retryDue(obj *ociv1alpha1.ImageBuild) bool {
	la := obj.Status.LastAttempt
	if la == nil || la.FinishedAt == nil {
		return true
	}
	return !time.Now().Before(la.FinishedAt.Add(failureBackoff(obj.Status.Failures)))
}

// maxFailureDetail is BuildAttempt.Message's MaxLength. Exceeding it makes the API server reject
// the whole status write.
const maxFailureDetail = 4096

// jobFailureDetail explains a failed build using the failing container's exit code and
// termination message; the Job condition alone says only "BackoffLimitExceeded".
func (r *ImageBuildReconciler) jobFailureDetail(ctx context.Context, obj *ociv1alpha1.ImageBuild, job *batchv1.Job) string {
	msg := jobFailureMessage(job)

	pods, err := r.buildPods(ctx, obj, job)
	if err != nil {
		return msg
	}
	return failureDetailFor(msg, pods.Items...)
}

// failureDetailFor turns the pods of a failed Job into the message stored in status, within
// maxFailureDetail.
func failureDetailFor(msg string, pods ...corev1.Pod) string {
	for _, p := range pods {
		// Init containers first: if one failed (e.g. the context fetch), the build never ran.
		statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...),
			p.Status.ContainerStatuses...)
		for _, cs := range statuses {
			t := cs.State.Terminated
			if t == nil || t.ExitCode == 0 {
				continue
			}
			where := fmt.Sprintf("container %q exited %d", cs.Name, t.ExitCode)
			if t.Reason != "" {
				where += " (" + t.Reason + ")"
			}
			// The pod goes with the Job on the next retry, so the cause itself must be in the message.
			hint := fmt.Sprintf("; see `kubectl -n %s logs %s -c %s` while the pod lasts",
				p.Namespace, p.Name, cs.Name)

			cause := strings.TrimSpace(t.Message)
			if cause == "" {
				return recon.Truncate(msg+": "+where+hint, maxFailureDetail)
			}
			// Cause first, and only the cause's head is trimmed. ADR 0046.
			suffix := " [" + where + "]" + hint
			budget := max(maxFailureDetail-len(suffix), 0)
			return recon.TruncateTail(cause, budget) + suffix
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
		// So an edited Dockerfile ConfigMap rebuilds now, not at the next interval.
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.buildsForConfigMap)).
		Complete(r)
}

// recordKept notes a build that was not published because onConflict: Keep left an existing tag
// alone.
//
// Conditions are left to applyOutcome and readyMessage. The outcome is Ready, not Stalled: an
// upstream change cannot raise the generation change Stalled needs. status.artifact is not
// synthesised from the tag, since nothing proves which inputs produced it.
func (r *ImageBuildReconciler) recordKept(obj *ociv1alpha1.ImageBuild, c *ociv1alpha1.TagConflictStatus) {
	obj.Status.Conflict = c
	obj.Status.Failures = 0
	// Nothing is in flight any more, even when the kept tag was found after a Job ran.
	obj.Status.BuildRef = nil
	recon.Event(r.Recorder, obj, corev1.EventTypeNormal, ociv1alpha1.ReasonSucceeded,
		fmt.Sprintf("Kept %s at %s; no build was run (onConflict: Keep)", c.Tag, c.Existing))
}

// attestationMode summarises the BuildKit attestation options for the input hash.
func attestationMode(cfg JobConfig) string {
	switch {
	case cfg.SBOM && cfg.Provenance:
		return "sbom+provenance"
	case cfg.SBOM:
		return "sbom"
	case cfg.Provenance:
		return "provenance"
	default:
		return ""
	}
}

// maxDockerfileFromConfigMap bounds one key, like build.FetchDockerfile's bound on a context.
const maxDockerfileFromConfigMap = 1 << 20

// dockerfileFromConfigMap reads the Dockerfile out of one ConfigMap key. Every failure is Pending,
// not Terminal: the fix is in the ConfigMap, which is watched.
func (r *ImageBuildReconciler) dockerfileFromConfigMap(
	ctx context.Context, obj *ociv1alpha1.ImageBuild,
) ([]byte, error) {
	ref := obj.Spec.Dockerfile.ConfigMapRef
	key := ref.Key
	if key == "" {
		key = "Dockerfile"
	}

	// Always obj.Namespace: the reference carries none (threat-model I4).
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: obj.Namespace, Name: ref.Name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, recon.Pending("ConfigMap %s/%s not found", obj.Namespace, ref.Name)
		}
		return nil, fmt.Errorf("reading ConfigMap %s/%s: %w", obj.Namespace, ref.Name, err)
	}

	content, ok := cm.Data[key]
	if !ok {
		if raw, binary := cm.BinaryData[key]; binary {
			return boundedDockerfile(raw, ref.Name, key)
		}
		return nil, recon.Pending("ConfigMap %s/%s has no key %q", obj.Namespace, ref.Name, key)
	}
	return boundedDockerfile([]byte(content), ref.Name, key)
}

func boundedDockerfile(content []byte, name, key string) ([]byte, error) {
	if len(content) > maxDockerfileFromConfigMap {
		return nil, recon.Pending("ConfigMap %s key %q is %d bytes, over the %d-byte limit for a "+
			"Dockerfile", name, key, len(content), maxDockerfileFromConfigMap)
	}
	return content, nil
}

// buildsForConfigMap maps a changed ConfigMap to the builds in its namespace that read a
// Dockerfile from it.
func (r *ImageBuildReconciler) buildsForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	var list ociv1alpha1.ImageBuildList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing ImageBuilds for a ConfigMap change")
		return nil
	}

	var out []reconcile.Request
	for i := range list.Items {
		df := list.Items[i].Spec.Dockerfile
		if df == nil || df.ConfigMapRef == nil || df.ConfigMapRef.Name != obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			},
		})
	}
	return out
}

// refreshNow renews the lease on what was just published, which otherwise has none until the next
// cycle (zot may even carry an old timestamp onto a new tag). Best effort; never fatal.
func (r *ImageBuildReconciler) refreshNow(ctx context.Context, obj *ociv1alpha1.ImageBuild) {
	if r.Refresher == nil {
		return
	}
	r.Refresher.RefreshNow(ctx, retention.Target{
		Object: obj, Push: obj.Spec.Push,
		Artifact: obj.Status.Artifact, History: obj.Status.History,
	})
}

// exportRef publishes status.artifact into the push.writeRefTo ConfigMap. No-op until there is an
// artifact.
func (r *ImageBuildReconciler) exportRef(ctx context.Context, obj *ociv1alpha1.ImageBuild) error {
	if obj.Spec.Push.GetWriteRefTo() == nil || obj.Status.Artifact == nil {
		return nil
	}
	written, err := recon.ExportRef(ctx, r.Client, obj, obj.Spec.Push.WriteRefTo, r.Export,
		obj.Status.Artifact.Digest, obj.Status.Artifact.Ref)
	if err != nil {
		return err
	}
	return r.recordExport(ctx, obj, written)
}

// recordExport records where the ConfigMap was written (status is the only record of its
// namespace) and removes the previous one if it moved. ADR 0056.
func (r *ImageBuildReconciler) recordExport(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, written *ociv1alpha1.RefExportStatus,
) error {
	if _, err := recon.RecordExport(ctx, r.Client, obj, obj.Status.RefExport, written); err != nil {
		return err
	}
	// Mutate, do not patch: an intermediate status patch overwrites the in-memory status with the
	// server's, discarding unpersisted fields such as Artifact and InputHash, so the object never
	// converges.
	obj.Status.RefExport = written
	return nil
}

// pollInterval is how often a running Job is re-observed.
func (r *ImageBuildReconciler) pollInterval() time.Duration {
	if r.BuildPollInterval > 0 {
		return r.BuildPollInterval
	}
	return defaultBuildPollInterval
}

// reconcileExportLifecycle keeps the finalizer in step with whether there is anything to clean up.
// Only an export into another namespace needs one: an own-namespace export is garbage-collected
// through its owner reference. done is true when this reconcile should stop.
func (r *ImageBuildReconciler) reconcileExportLifecycle(
	ctx context.Context, obj *ociv1alpha1.ImageBuild, exp *ociv1alpha1.RefExport,
) (ctrl.Result, bool, error) {
	has := controllerutil.ContainsFinalizer(obj, ociv1alpha1.Finalizer)

	if !obj.DeletionTimestamp.IsZero() {
		if !has {
			return ctrl.Result{}, true, nil
		}
		// Only the export needs explicit cleanup; Secrets and Jobs are owner-collected.
		if err := recon.DeleteExportedRef(ctx, r.Client, obj, obj.Status.RefExport); err != nil {
			return ctrl.Result{}, true, err
		}
		patch := client.MergeFrom(obj.DeepCopy())
		controllerutil.RemoveFinalizer(obj, ociv1alpha1.Finalizer)
		return ctrl.Result{}, true, client.IgnoreNotFound(r.Patch(ctx, obj, patch))
	}

	// Remove an export no longer asked for now: a stale ConfigMap is worse than none.
	if exp == nil && obj.Status.RefExport != nil {
		if err := r.recordExport(ctx, obj, nil); err != nil {
			return ctrl.Result{}, true, err
		}
	}

	wantsFinalizer := exp != nil && exp.Namespace != obj.Namespace
	if wantsFinalizer == has {
		return ctrl.Result{}, false, nil
	}
	patch := client.MergeFrom(obj.DeepCopy())
	if wantsFinalizer {
		controllerutil.AddFinalizer(obj, ociv1alpha1.Finalizer)
	} else {
		controllerutil.RemoveFinalizer(obj, ociv1alpha1.Finalizer)
	}
	if err := r.Patch(ctx, obj, patch); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("updating finalizer: %w", err)
	}
	return ctrl.Result{}, false, nil
}
