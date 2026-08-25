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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/attest"
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

	// Default is the operator's registry and credential, used by builds that name no repository of
	// their own. See recon.DefaultRegistry.
	Default recon.DefaultRegistry

	// Recorder surfaces failures as Events. A build failure's detail lives in the pod's logs,
	// which vanish with the pod, so the Event is often the only durable trace of why.
	Recorder record.EventRecorder

	// Attestor signs the build's output, after the Job has terminated.
	//
	// The signing key stays in THIS process and is never projected into a build pod -- so code
	// that came out of a git repository never runs in the same container as the key. The SBOM and
	// provenance come from BuildKit instead, in-band, because only the build can see what it
	// installed.
	Attestor *attest.Attestor

	// Transport, when set, trusts an additional CA on top of the system roots. Same object the
	// other controllers use; see recon.Transport.
	Transport http.RoundTripper

	// HTTPClient fetches the build context for the Dockerfile check. Nil uses a default with a
	// timeout; the build itself never streams through this process.
	HTTPClient *http.Client

	// HistoryLimit is how many past builds are retained in status.
	HistoryLimit int

	// RequirePinnedSources refuses a spec.context that names no revision (threat T1). Off by
	// default, and it matters more here than on the composer: an unpinned context means the build
	// runs whatever code the branch happens to be at, and a build's output cannot be reproduced
	// from its spec (ADR 0025), so there is nothing to check it against afterwards.
	RequirePinnedSources bool
}

// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagebuilds,verbs=get;list;watch
// +kubebuilder:rbac:groups=oci.lhns.de,resources=imagebuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// Secrets are read for their resourceVersion only, so a rotation moves the input hash and
// rebuilds. The VALUE is never read here — it is projected straight into the build pod — which is
// why this is get and not list or watch, matching the composer's reasoning about blast radius.
// create and update are for ONE thing: a short-lived copy of the operator's registry credential in
// the namespace a build runs in, because a pod can only mount Secrets from its own namespace and the
// build must run beside the tenant's own build secrets and code. The copy is owned by the ImageBuild
// and goes with it.
//
// This is a real widening and it is not narrowable by RBAC: `create` cannot be restricted to a name.
// So this controller can create a Secret in any namespace, and update ones it names. What it still
// cannot do is LIST or WATCH them -- it can only touch Secrets whose names it already knows.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update
//
// ConfigMaps ARE cached, unlike Secrets, because a Dockerfile can live in one and it is watched --
// an edit must rebuild promptly rather than at the next interval, which defaults to an hour. That
// costs an informer over every ConfigMap in the cluster; the composer already pays it for configMap
// layers, for exactly this reason. The alternative is a controller that appears not to notice edits.
//
// get;list;watch and NOTHING else. Everything this controller WRITES into a tenant namespace is a
// Secret -- the Dockerfile copy included -- so no create or update appears here, and that asymmetry
// is deliberate rather than an oversight.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
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

	resolved, err := r.resolveContext(ctx, obj)
	if err != nil {
		return build.Inputs{}, "", err
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
	unpack  string
	art     source.FluxArtifact
}

// resolveContext resolves whichever member spec.context names.
//
// No context is a legal, empty tree: addressed by construction, so the "unaddressed context"
// objection never applied to it. CEL has already refused the one combination that cannot work --
// no context and a Dockerfile that is a path into one.
func (r *ImageBuildReconciler) resolveContext(ctx context.Context, obj *ociv1alpha1.ImageBuild) (contextInputs, error) {
	switch c := obj.Spec.Context; {
	case c.GetImage() != nil:
		img := c.GetImage()
		// The digest IS the reference, so there is nothing to resolve. Split out rather than
		// hashing the whole ref: the tag is decorative, as it is for spec.base.ref, and hashing it
		// would rebuild on a retag that pulls the same bytes.
		_, digest, _ := strings.Cut(img.Ref, "@")
		return contextInputs{kind: "image", subpath: img.Subpath,
			art: source.FluxArtifact{URL: img.Ref, Digest: digest}}, nil

	case c.GetFetch() != nil:
		f := c.GetFetch()
		// Nothing to resolve: the digest is DECLARED, which is what makes an arbitrary URL a legal
		// build input. The bytes are verified against it in the build pod, before anything is
		// unpacked -- see internal/fetchcontext.
		return contextInputs{kind: "fetch", subpath: f.Subpath, unpack: string(f.Unpack),
			art: source.FluxArtifact{URL: f.URL, Digest: f.Digest}}, nil

	case c.GetSourceRef() != nil:
		return r.resolveSourceRef(ctx, obj, c.GetSourceRef())

	default:
		return contextInputs{}, nil
	}
}

func (r *ImageBuildReconciler) resolveSourceRef(ctx context.Context, obj *ociv1alpha1.ImageBuild,
	ref *ociv1alpha1.SourceRefSource) (contextInputs, error) {
	// Same namespace only, for the reason the composer refuses it: the RBAC is cluster-wide, so
	// naming another namespace's source would let anyone who can create an ImageBuild read content
	// they have no access to.
	if ref.Namespace != "" && ref.Namespace != obj.Namespace {
		return contextInputs{}, recon.Terminal(
			"build context %s/%s is in namespace %q: a context must be in the same namespace as "+
				"the ImageBuild that consumes it", ref.Kind, ref.Name, ref.Namespace)
	}
	// Threat-model gap T1: an unpinned context builds whatever the branch is at now, and an
	// ImageBuild's output is an observation rather than a function of its spec (ADR 0025), so
	// nothing afterwards can tell you what went in. Terminal -- editing this spec is the fix.
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

	// Same rule as the composer's layers: an explicit revision waits for the source to reach it
	// rather than building from whatever is currently published.
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

// resolveDockerfile decides what identifies the recipe.
//
// A path needs no content hashing: it lives inside the context, which ContextDigest already
// addresses, so an edit moves the hash and no fetch happens on a reconcile about to short-circuit.
// The other two forms ride in no tarball, so their bytes are hashed directly.
func (r *ImageBuildReconciler) resolveDockerfile(ctx context.Context, obj *ociv1alpha1.ImageBuild) (dockerfileInputs, error) {
	df := obj.Spec.Dockerfile

	switch {
	case df != nil && df.Inline != "":
		return dockerfileInputs{kind: "inline", digest: sha256Hex([]byte(df.Inline))}, nil

	case df != nil && df.ConfigMapRef != nil:
		// A ConfigMap is mutable by construction, so it can never satisfy the pinning flag. Refused
		// rather than ignored: a flag that silently does not apply to one source is worse than one
		// that says so.
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

// dockerfileBytes returns the Dockerfile this build will run, and whether it came from the spec.
//
// One function so the FROM check has exactly one input to guard, whatever the source. The flag is
// returned rather than re-derived at the call site because it decides terminality, and a second
// place deciding that is a second place to get it wrong.
func (r *ImageBuildReconciler) dockerfileBytes(ctx context.Context, obj *ociv1alpha1.ImageBuild,
	contextURL string) (content []byte, inline bool, err error) {

	if df := obj.Spec.Dockerfile; df != nil && df.Inline != "" {
		return []byte(df.Inline), true, nil
	}
	if df := obj.Spec.Dockerfile; df != nil && df.ConfigMapRef != nil {
		// Read again rather than threaded down from resolveInputs: the second read is what the Job
		// gets, so a ConfigMap edit in between just moves the hash on the next pass. Threading it
		// would add a parameter whose only job is to be kept in step.
		content, err := r.dockerfileFromConfigMap(ctx, obj)
		// Not inline: the fix for an unpinned FROM here is editing the ConfigMap, which raises no
		// generation change on this object, exactly like a Dockerfile in the context.
		return content, false, err
	}

	// An image context is checked by the fetcher instead -- reading one file out of an image here
	// would mean registry credentials for arbitrary user-named repositories in a controller shared
	// by every namespace. The guard moves rather than being skipped; see internal/fetchcontext.
	if obj.Spec.Context.GetImage() != nil {
		return nil, false, nil
	}

	var subpath string
	stripWrapper := true
	if ref := obj.Spec.Context.GetSourceRef(); ref != nil {
		subpath = ref.Subpath
	}
	if f := obj.Spec.Context.GetFetch(); f != nil {
		// No unpredictable wrapper to strip in a fetched archive -- `subpath` names a version-named
		// one. Stripping anyway would look past the real top-level directory.
		subpath, stripWrapper = f.Subpath, false
	}
	content, err = build.FetchDockerfile(ctx, r.httpClient(), contextURL,
		subpath, obj.Spec.Dockerfile.EffectiveDockerfile(), stripWrapper)
	if err != nil {
		return nil, false, fmt.Errorf("reading the Dockerfile: %w", err)
	}
	return content, false, nil
}

// startBuild validates the Dockerfile and creates the Job.
func (r *ImageBuildReconciler) startBuild(ctx context.Context, obj *ociv1alpha1.ImageBuild,
	inputs build.Inputs, inputHash, contextURL string) error {

	// The FROM check happens here rather than inside the Job, so an unpinned base is refused before
	// anything executes. For a path that costs one fetch of the context, but only where a build is
	// about to run anyway -- never on the reconcile that finds an unchanged hash.
	dockerfile, inline, err := r.dockerfileBytes(ctx, obj, contextURL)
	if err != nil {
		return err
	}
	// Nil is not "skip the guard": it is an image context, which the fetcher checks instead.
	if dockerfile != nil {
		if err := build.CheckPinnedBases(bytes.NewReader(dockerfile)); err != nil {
			if inline {
				// Terminal only here: the Dockerfile IS this spec, so the fix is an edit, and the
				// generation change it raises is what wakes the object. A Dockerfile in another
				// object raises no such event.
				return recon.Terminal("%s", err)
			}
			return fmt.Errorf("%w", err)
		}
	}

	// The credential exists before the pod that mounts it. Both names come from jobName, so there is
	// one source of truth for what this build is called.
	pushSecret, err := r.pushSecretFor(ctx, obj, jobName(obj, inputHash))
	if err != nil {
		return err
	}
	// Same lifetime, same owner, same reason: a pod mounts only from its own namespace.
	caSecret, err := r.registryCASecretFor(ctx, obj, jobName(obj, inputHash))
	if err != nil {
		return err
	}
	// Only when the Dockerfile does not ride inside the context. These are the exact bytes checked
	// above, which is what stops the pod building something that was never checked.
	//
	// Keyed off the same predicate the Job rendering uses, not off `inline`: a ConfigMap Dockerfile
	// is not inline but is still projected, and keying these two off different conditions is how a
	// mount and its `--local` stop agreeing.
	var dockerfileSecret string
	if projectedDockerfile(obj) {
		dockerfileSecret, err = r.dockerfileSecretFor(ctx, obj, jobName(obj, inputHash), dockerfile)
		if err != nil {
			return err
		}
	}

	job := buildJob(obj, inputHash, contextURL, inputs.ContextDigest, r.JobConfig, r.repositoryFor(obj), pushSecret,
		caSecret, dockerfileSecret, r.cacheAvailable(ctx, obj))
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
		// After recordSuccess, so status already names what was built when signing looks at it.
		obj.Status.Attestations = r.signBuild(ctx, obj, digest)
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
	// The PULL name, which is the only place it is used. Everything that connects -- the Job's push
	// target, the tag-conflict check, the cache reference, the retention refresh -- goes through
	// repositoryFor, because those run from inside the cluster where the public name may not
	// resolve. status.artifact is the one thing a workload reads, so it is the one thing that gets
	// the public name.
	repo := r.Default.PublicRepository(r.repositoryFor(obj))
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
	}
	// Where the build came from, so an artifact can be traced back to a revision without pulling it
	// apart. The composer records this per layer; a build has at most one context.
	//
	// Omitted rather than recorded empty when there is no context: a build whose Dockerfile is its
	// whole input has no source to trace to, and an entry with three empty fields would claim
	// otherwise.
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
		// Init containers FIRST, and including them at all is the fix. This iterated only
		// ContainerStatuses, which does not contain them, so a context that failed to fetch
		// reported "BackoffLimitExceeded" and nothing else -- the mechanism, with the cause
		// discarded. First because an init container failing means the build container never ran,
		// so its status carries nothing worth preferring.
		statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...),
			p.Status.ContainerStatuses...)
		for _, cs := range statuses {
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
		// Without this a ConfigMap holding a Dockerfile would only be noticed at the next interval,
		// an hour by default. Editing the recipe and watching nothing happen reads as the controller
		// being broken. Same reasoning, and near enough the same code, as the composer's watch on
		// configMap layers.
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.buildsForConfigMap)).
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

// attestationMode summarises the BuildKit attestation options for the input hash. A string rather
// than two booleans so that adding a third option later cannot silently collide with an existing
// combination.
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

// maxDockerfileFromConfigMap bounds one key.
//
// The same bound build.FetchDockerfile applies to a Dockerfile pulled out of a context tarball, for
// the same reason: a Dockerfile is kilobytes, and reading more than that into a controller shared by
// every namespace is a way to make that controller someone else's problem. A whole ConfigMap is
// capped near 1 MiB by etcd, so this is close to that ceiling rather than far below it -- the point
// is a legible error rather than a surprising one.
const maxDockerfileFromConfigMap = 1 << 20

// dockerfileFromConfigMap reads the Dockerfile out of one ConfigMap key.
//
// Every failure here is Pending rather than Terminal, because every fix is in the OTHER object:
// create the ConfigMap, add the key, shrink it. Terminal would wedge this object while the thing it
// needs sits there waiting to be fixed, and ConfigMaps are watched, so the wait is usually over the
// moment one appears.
func (r *ImageBuildReconciler) dockerfileFromConfigMap(
	ctx context.Context, obj *ociv1alpha1.ImageBuild,
) ([]byte, error) {
	ref := obj.Spec.Dockerfile.ConfigMapRef
	key := ref.Key
	if key == "" {
		key = "Dockerfile"
	}

	// obj.Namespace, never a namespace from the spec. The reference carries none, and that is the
	// whole of the boundary (threat-model I4).
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

// buildsForConfigMap maps a changed ConfigMap to the builds that read a Dockerfile from it.
//
// Namespace-scoped: a configMapRef resolves in the object's own namespace, so a same-named ConfigMap
// elsewhere is unrelated and must not trigger a rebuild. Contrast the composer's source mapping,
// which lists cluster-wide because a sourceRef carries a namespace field.
//
// A linear scan rather than a field index, matching the composer: the list is namespace-scoped and
// the comparison is one string, so an index would be machinery for a loop that is already small.
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
