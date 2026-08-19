package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Condition types and reasons, following the kstatus conventions Flux uses so that
// `kubectl wait --for=condition=Ready`, `flux get` and notification-controller all behave the
// way users of a Flux-ecosystem controller expect.
const (
	// ReadyCondition is the top-level summary condition.
	ReadyCondition = "Ready"
	// ReconcilingCondition signals work in progress; a transient failure keeps this set so the
	// object is retried with backoff.
	ReconcilingCondition = "Reconciling"
	// StalledCondition signals a TERMINAL error that retrying cannot fix — an invalid spec, a
	// digest mismatch, a refusal to overwrite immutable content.
	//
	// The test for belonging here is narrow: editing THIS object's spec must be what fixes it.
	// That is what makes stalling safe, because the resulting generation change is the event
	// that wakes the controller back up. A failure fixed by changing anything else — a Secret,
	// a Flux source, the operator's own configuration — must not stall, because no such event
	// will ever arrive. See ReasonDependencyNotReady.
	StalledCondition = "Stalled"
)

// Reasons attached to the conditions above. Kept as constants so tests can assert on them
// rather than on message text.
const (
	ReasonSucceeded         = "Succeeded"
	ReasonProgressing       = "Progressing"
	ReasonDigestMismatch    = "DigestMismatch"
	ReasonInvalidSpec       = "InvalidSpec"
	ReasonImmutableConflict = "ImmutableTagConflict"
	ReasonFetchFailed       = "FetchFailed"
	ReasonPublishFailed     = "PublishFailed"
	ReasonSuspended         = "Suspended"

	// ReasonBuildFailed covers an ImageBuild whose Job did not succeed. Never sets Stalled: the fix
	// lives in another object, so no generation change would arrive to wake it up.
	ReasonBuildFailed = "BuildFailed"

	// ReasonDependencyNotReady covers something the composition refers to that does not exist
	// yet, or exists but cannot be used: a Flux source, a Secret, a non-optional ConfigMap, or
	// a serving endpoint the operator was never given.
	//
	// Kept off Stalled deliberately — see StalledCondition. Applying a composition together
	// with its GitRepository in one commit is enough to hit this: whichever loses the race
	// would otherwise stay wedged forever while the thing it needs sits there Ready.
	ReasonDependencyNotReady = "DependencyNotReady"
)

// Finalizer is set on objects so published artifacts can be cleaned up on delete.
// ReconcileRequestAnnotation is Flux's key, not one of ours, because the key IS the contract:
// `flux reconcile` writes this one and asks nobody. Controllers echo it into
// status.lastHandledReconcileAt once acted on, which is how a client knows the request landed.
const ReconcileRequestAnnotation = "reconcile.fluxcd.io/requestedAt"

const Finalizer = "finalizers.oci.lhns.de"

// LocalObjectReference refers to an object in the same namespace. Credentials are always
// referenced, never inlined.
type LocalObjectReference struct {
	// Name of the referent.
	// +required
	Name string `json:"name"`
}

// Publish describes where the artifact is made available when no external registry is used.
//
// This is the DEFAULT mode and it requires no registry, no credentials and no node
// configuration: the controller serves the artifact from its own read-only OCI distribution
// endpoint. Deterministic assembly is what makes this cheap — the controller holds no durable
// state, because it can always re-assemble the artifact from the spec.
type Publish struct {
	// Name is the repository path the artifact is served under, e.g. "kafka-tiered-storage"
	// results in <serving-host>/kafka-tiered-storage.
	// +required
	Name string `json:"name"`

	// Tags are the tags this build is published under, in order. Optional, and defaulted to
	// nothing: with no tags the artifact is published by digest alone, which is all a workload
	// pinned by image automation needs.
	//
	// The intended use is a SPEC-HASH tag — a hash of the build-determining part of this spec,
	// computed by whatever templates it, written into both this field and the consuming
	// workload's image reference. Because assembly is deterministic (ADR 0016), such a tag
	// identifies the output as precisely as its digest does, and both sides derive it from one
	// source with nothing observing anything at runtime. See ADR 0017.
	//
	// A list, so one build can carry several: a spec-hash tag alongside a readable pointer, or
	// the same hash under more than one algorithm.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9._-]*$`
	// +optional
	Tags []string `json:"tags,omitempty"`

	// Ref is a full image reference whose TAG is added to Tags. The host and repository path are
	// parsed and then ignored: the controller already knows its own serving address, and the
	// repository comes from Name.
	//
	// It exists so the tag can be set by whatever already rewrites image references, rather than
	// needing a second mechanism. kustomize's images transformer, for one, rewrites a scalar
	// image field wherever a fieldSpec points at it — so one entry can retag this artifact AND
	// the workload that consumes it, keeping them in step by construction:
	//
	//   images:
	//     - name: my-artifact                       # matches this field and the consumer's
	//       newName: registry.example/my-artifact
	//       newTag: s1a2b3c4d5e6f7890
	//
	// A ref with NO tag contributes nothing rather than failing. That is what an untemplated
	// manifest looks like, and it degrades to publishing by digest alone — which is supported.
	// The consumer's reference would be equally unrewritten, so the mistake surfaces at the pod
	// rather than being hidden here.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Ref string `json:"ref,omitempty"`

	// Immutable refuses to move a tag that already resolves to different content, failing the
	// build instead of changing what the tag means. Defaults to TRUE, because silently
	// remeaning a tag is what leaves nodes running different bytes under one name.
	//
	// Republishing IDENTICAL content is always a no-op regardless of this field, so a steady
	// reconcile loop never trips it — only a real change of meaning does.
	//
	// Set false for a deliberately moving pointer, e.g. tags: [main] repointed at every build.
	//
	// A pointer because the zero value is meaningful: a plain bool with omitempty would drop an
	// explicit `false` on the wire and let the default quietly win.
	// +kubebuilder:default=true
	// +optional
	Immutable *bool `json:"immutable,omitempty"`

	// History is how many past builds to keep before garbage collection reclaims their blobs.
	// Defaults to the controller's --gc-keep-builds.
	//
	// Old builds are worth keeping for three independent reasons: reverting a commit must find
	// the old reference still pullable; a pod pinned to one that gets rescheduled must be able
	// to pull it again; and with spec-hash tags a rollback names a tag from an earlier build,
	// which only resolves while that build is retained. Layers are shared between builds, so a
	// generous value costs far less than the count suggests. See ADR 0011.
	// +kubebuilder:validation:Minimum=1
	// +optional
	History *int32 `json:"history,omitempty"`
}

// GetTags is nil-safe, because spec.publish may be omitted entirely while the controller still
// has a serving endpoint to publish to.
func (p *Publish) GetTags() []string {
	if p == nil {
		return nil
	}
	return p.Tags
}

// GetRef is nil-safe, for the same reason as GetTags. Parsing lives in the controller rather than
// here so this package keeps its dependency surface small for anyone importing the types.
func (p *Publish) GetRef() string {
	if p == nil {
		return ""
	}
	return p.Ref
}

// TagsAreImmutable resolves the optional Immutable field, which defaults to true.
//
// The API server applies that default, so nil should only be seen for objects written before
// the field existed or for structs built directly in tests. Defaulting to the safe answer here
// too means neither case can quietly end up with unprotected tags.
func (p *Publish) TagsAreImmutable() bool {
	return p == nil || p.Immutable == nil || *p.Immutable
}

// TagsAreImmutable is the Push equivalent, deliberately identical to Publish's.
func (p *Push) TagsAreImmutable() bool {
	return p == nil || p.Immutable == nil || *p.Immutable
}

// SourceRecord is where one layer's content came from.
type SourceRecord struct {
	// Name of the layer, matching spec.layers[].name.
	// +optional
	Name string `json:"name,omitempty"`

	// Revision the content was resolved at, for a source that has one — a Flux artifact's
	// "main@sha1:abcd". This is the field that answers "which commit is in this image?".
	// +optional
	Revision string `json:"revision,omitempty"`

	// Digest of the resolved content: the declared digest of a fetch, the artifact digest of a
	// Flux source, the manifest digest of an image layer.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// DefaultHistoryLimit is how many past builds are retained when nothing says otherwise.
//
// Not 1, and not unbounded. Layers are shared between builds so the marginal cost of retaining one
// is small, while the cost of having reclaimed one too eagerly is a workload that cannot pull the
// digest it is pinned to. See ADR 0011. Shared by both kinds, so the number and its reasoning stay
// in one place.
const DefaultHistoryLimit = 10

// BuildRecord is one past build, retained so garbage collection knows what is still live.
//
// Kept in status rather than inferred from what is in storage. Inference would mean deciding an
// object is garbage because nothing appears to point at it, which is exactly the reasoning that
// deletes live data when the controller's view is incomplete. An explicit record also makes
// retention visible in `kubectl get -o yaml`.
type BuildRecord struct {
	// Tags this build was published under, if any. Replayed after a restart so the references a
	// workload names keep resolving; a build with no tags is still replayed by digest.
	// +optional
	Tags []string `json:"tags,omitempty"`

	// Digest of the manifest. For a multi-platform build this is the INDEX.
	// +optional
	Digest string `json:"digest,omitempty"`

	// Blobs are the config and layer digests this build is composed of. These are the objects
	// garbage collection must not reclaim while this build is retained. For a multi-platform
	// build it is the union across every child.
	// +optional
	Blobs []string `json:"blobs,omitempty"`

	// Sources records what each layer was resolved FROM, so an artifact can be traced back to the
	// revision that produced it.
	//
	// Without this the only way to answer "which revision is in this image?" is to pull the
	// manifest, fetch the layer and read its contents — which is how the incident behind ADR 0026
	// had to be diagnosed, and why a wrong artifact sat unnoticed until a tag conflict surfaced it.
	// +optional
	Sources []SourceRecord `json:"sources,omitempty"`

	// InputHash the build was produced from. Written by ImageBuild only; ImageComposition's
	// identity is the output digest rather than the hash. See ADR 0025.
	// +optional
	InputHash string `json:"inputHash,omitempty"`

	// Manifests are the CHILD manifest digests when this build is a multi-platform index. Empty
	// for a single-platform build, where Digest is the manifest itself.
	//
	// Garbage collection reads this, and must: without it the index is retained while its children
	// are swept, leaving a manifest that resolves to nothing. That is indistinguishable from
	// having deleted the artifact, except that it fails at pull time rather than at collection
	// time, long after the change that caused it.
	// +optional
	Manifests []string `json:"manifests,omitempty"`

	// Time the build was published.
	// +optional
	Time *metav1.Time `json:"time,omitempty"`
}

// Push describes an external registry to publish to. Optional: omit it to use the built-in
// serving endpoint instead.
type Push struct {
	// Repository is the fully qualified target, e.g. "ghcr.io/example/artifact".
	// +required
	Repository string `json:"repository"`

	// Tags are the tags to push, as described on Publish.Tags. Empty pushes by digest only.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9._-]*$`
	// +optional
	Tags []string `json:"tags,omitempty"`

	// SecretRef names a docker-registry Secret holding push credentials.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// Immutable behaves exactly as on Publish, including the default of true. Identical on both
	// so the field cannot mean different things depending on where it sits — and an external
	// registry is where a silently remeaned tag does the most damage.
	// +kubebuilder:default=true
	// +optional
	Immutable *bool `json:"immutable,omitempty"`
}

// ArtifactStatus records what was produced. Deliberately identical in shape across every kind
// in this API group so consumers need not care which controller produced it.
type ArtifactStatus struct {
	// Digest of the published manifest, e.g. "sha256:...". This is the value to pin.
	// +optional
	Digest string `json:"digest,omitempty"`

	// Revision is the human-facing "<tag>@<digest>", using the first tag. Just the digest when
	// the build carries no tags.
	// +optional
	Revision string `json:"revision,omitempty"`

	// Ref is the complete pullable reference, including the serving host in serving mode.
	// Surfaced as a printer column so `kubectl get` shows the exact string to use.
	// +optional
	Ref string `json:"ref,omitempty"`

	// Tags this artifact is published under, fully qualified. Empty when published by digest
	// only. What a workload should name is a tag chosen by whatever templates the spec, not a
	// value read back from here — see ADR 0017.
	// +optional
	Tags []string `json:"tags,omitempty"`

	// LastUpdateTime is when this artifact was last published.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`
}

// Conditions accessors, so the shared reconcile helpers can write conditions on either kind
// without knowing which it holds. The shape follows Flux's own ObjectWithConditions.

func (o *ImageComposition) GetConditions() []metav1.Condition  { return o.Status.Conditions }
func (o *ImageComposition) SetConditions(c []metav1.Condition) { o.Status.Conditions = c }

func (o *ImageBuild) GetConditions() []metav1.Condition  { return o.Status.Conditions }
func (o *ImageBuild) SetConditions(c []metav1.Condition) { o.Status.Conditions = c }
