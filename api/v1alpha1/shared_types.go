package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	ReasonAttestationFailed = "AttestationFailed"
	ReasonSuspended         = "Suspended"
	// ReasonRetentionDegraded reports that the refresh keeping this object's images from being
	// reclaimed has been failing. See ADR 0031: the refresh fails UNSAFE, so sustained failure
	// ends in deletion rather than in a stuck object, and this is the warning before that.
	ReasonRetentionDegraded = "RetentionDegraded"

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

// ResolveConflictPolicy returns the effective policy for a tag that already resolves to something
// else, reconciling the three-valued field with the deprecated two-valued one.
//
// Precedence is onConflict, then immutable, then Fail. That order is what makes the upgrade
// non-breaking in both directions: an object written before onConflict existed still has its
// `immutable` honoured, and one that sets onConflict is not second-guessed by an `immutable` the
// API server materialised under it. Contradictions are refused by CEL before they are stored.
//
// Nil is Fail rather than Overwrite: a struct built in a test, or a spec with no push block at all,
// must not end up with unprotected tags by omission.
func (p *Push) ResolveConflictPolicy() TagConflictPolicy {
	if p == nil {
		return ConflictFail
	}
	return resolveConflict(p.OnConflict, p.Immutable)
}

func resolveConflict(explicit TagConflictPolicy, deprecated *bool) TagConflictPolicy {
	if explicit != "" {
		return explicit
	}
	if deprecated != nil && !*deprecated {
		return ConflictOverwrite
	}
	return ConflictFail
}

// GetTags is nil-safe, because spec.push may be omitted entirely -- an object that names no
// repository and no tags publishes by digest to the operator's default registry.
func (p *Push) GetTags() []string {
	if p == nil {
		return nil
	}
	return p.Tags
}

func (p *Push) GetRef() string {
	if p == nil {
		return ""
	}
	return p.Ref
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

// +kubebuilder:validation:XValidation:rule="!(has(self.immutable) && has(self.onConflict)) || (self.immutable && self.onConflict == 'Fail') || (!self.immutable && self.onConflict == 'Overwrite')",message="immutable and onConflict contradict each other: immutable true means onConflict Fail, immutable false means onConflict Overwrite. immutable is deprecated; prefer setting onConflict alone."
// Push describes an external registry to publish to. Optional: omit it to use the built-in
// serving endpoint instead.
type Push struct {
	// Repository is the fully qualified target, e.g. "ghcr.io/example/artifact".
	//
	// Optional. Omitted, the object publishes to the operator's default registry under
	// <namespace>/<name> -- which is what a default chart install configures, so nothing here has
	// to name a host at all.
	//
	// Naming one has a second effect worth knowing: the operator's own registry credential is used
	// ONLY for the default target. An object that chooses its own repository authenticates with its
	// own secretRef, or not at all. Otherwise anyone able to create one of these objects could
	// point it at a host they control and have the controller hand over the operator's password.
	// +optional
	Repository string `json:"repository,omitempty"`

	// Tags are the tags to push, as described on Publish.Tags. Empty pushes by digest only.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9._-]*$`
	// +optional
	Tags []string `json:"tags,omitempty"`

	// SecretRef names a docker-registry Secret holding push credentials.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// Ref behaves exactly as on Publish: a reference whose TAG is appended to Tags, so a
	// kustomize images transformer or a Helm value can retag without editing this list.
	//
	// Absent until now, which meant the spec-hash tag pattern documented for ImageComposition
	// could not be used from ImageBuild at all — the one place a generator most wants it, since a
	// build's tag is the only thing identifying which inputs produced it.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Ref string `json:"ref,omitempty"`

	// History is how many past builds to retain, overriding the controller's default. Identical
	// to Publish's.
	//
	// It matters MORE here than there, not less: a composition can rebuild any artifact from its
	// spec, so retention is a convenience. A build cannot (ADR 0025), so retention is how much of
	// the only copy is kept. That this knob existed only on the kind where it matters least was an
	// oversight rather than a decision.
	// +kubebuilder:validation:Minimum=1
	// +optional
	History *int32 `json:"history,omitempty"`

	// Immutable is DEPRECATED on this side too; use OnConflict. Identical to Publish's in every
	// respect, so the field cannot mean different things depending on where it sits.
	//
	// It was also INERT here until the release that added OnConflict: nothing in the build
	// controller read it, and BuildKit pushed over whatever the tag held. The CRD advertised a
	// guarantee that did not exist.
	// +optional
	Immutable *bool `json:"immutable,omitempty"`

	// OnConflict decides what happens when a tag already resolves to different content: Fail
	// (refuse and stall), Overwrite (move the tag), or Keep (leave it, drop this build, stay
	// Ready). Defaults to Fail.
	//
	// Republishing IDENTICAL content is a no-op regardless of this field, so a steady reconcile
	// loop never reaches it -- only a real change of meaning does.
	//
	// Deliberately carries NO schema default, unlike the `immutable` field it replaces. Structural
	// defaults are applied when an object is read back from storage, so defaulting this would
	// rewrite every existing `immutable: false` object into a refusing one the moment the CRD was
	// upgraded -- a silent reversal of a setting its author chose on purpose. The effective default
	// lives in ResolveConflictPolicy instead, where it can consult `immutable` first.
	// +optional
	OnConflict TagConflictPolicy `json:"onConflict,omitempty"`
}

// TagConflictPolicy decides what happens when a tag already resolves to content other than what
// this spec produces.
//
// The two-valued `immutable` it replaces could only refuse or overwrite. Neither is right for the
// pattern this project actually recommends -- a tag derived from a hash of the spec -- where a tag
// that already exists means the content is ALREADY PUBLISHED and correct. Refusing stalls the
// object over a non-problem; overwriting rewrites bytes that were already right, and on the build
// side, where the output is not a function of the spec, replaces good content with a different
// build of the same inputs.
// +kubebuilder:validation:Enum=Fail;Overwrite;Keep
type TagConflictPolicy string

const (
	// ConflictFail refuses to change what a tag means, and stalls. The default, because silently
	// remeaning a tag is what leaves nodes running different bytes under one name.
	ConflictFail TagConflictPolicy = "Fail"
	// ConflictOverwrite moves the tag. For a deliberately moving pointer, e.g. tags: [main].
	ConflictOverwrite TagConflictPolicy = "Overwrite"
	// ConflictKeep leaves the existing tag alone, drops what this reconcile produced, and reports
	// Ready. The digest that was dropped is recorded in status, because otherwise status.artifact
	// stops describing what the spec produces while the object reads healthy -- which is the exact
	// shape of the incident behind ADR 0026.
	ConflictKeep TagConflictPolicy = "Keep"
)

// TagConflictStatus records content this object produced and did NOT publish, because
// onConflict: Keep left an existing tag in place.
//
// This exists so that Keep cannot become a silent divergence. Without it, status.artifact would go
// on describing content that is not what the current spec produces, while the object reads Ready
// and nothing anywhere says the two disagree. That is exactly the shape of the incident behind
// ADR 0026, where a served layer and the version it claimed to be had drifted apart and no field
// could adjudicate.
//
// Cleared as soon as a reconcile publishes without conflict, so it always describes the CURRENT
// divergence rather than accumulating history.
type TagConflictStatus struct {
	// Tag that was left alone.
	// +optional
	Tag string `json:"tag,omitempty"`

	// Existing is the digest the tag resolves to -- what consumers actually get.
	// +optional
	Existing string `json:"existing,omitempty"`

	// Dropped is the digest this spec produced and discarded. On a composition it is a pure
	// function of the spec, so it can be reproduced at will; on a build it cannot, and the content
	// is gone.
	// +optional
	Dropped string `json:"dropped,omitempty"`

	// ObservedAt is when the divergence was last seen.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// AttestationStatus records what was attached to an artifact, and to which artifact.
//
// It exists to make a converged reconcile cost NOTHING. ADR 0008 says the controller should verify
// the referrers exist and create only what is missing, which taken literally means a registry
// round trip per object per interval forever. This record answers the same question from etcd,
// checked as one more conjunct after the input-hash and published-digest checks a converged
// reconcile already performs.
//
// Trusting a status field about the registry is safe here for a specific reason: the attestations
// live in the SAME repository, under the SAME retention policy, as an artifact whose presence was
// just confirmed. A registry that lost the referrers lost the artifact too, so the digest check
// fails first and everything is re-derived from the registry.
//
// Subject is what invalidates it: a new artifact digest means these describe something else.
type AttestationStatus struct {
	// Subject is the artifact digest these attestations describe.
	Subject string `json:"subject,omitempty"`
	// SBOM is the manifest digest of the SPDX referrer.
	SBOM string `json:"sbom,omitempty"`
	// Provenance is the manifest digest of the SLSA referrer.
	Provenance string `json:"provenance,omitempty"`
	// Signature is the manifest digest of the cosign signature.
	Signature string `json:"signature,omitempty"`
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

// SourceRefSource takes content from a Flux source's artifact.
//
// source-controller already clones, tracks revisions and publishes a digest-addressed tarball, so
// this consumes that rather than forming a second opinion about what the repository contains. The
// digest is resolved from status.artifact. See ADR 0002.
type SourceRefSource struct {
	// Kind of the referenced source.
	// +kubebuilder:validation:Enum=GitRepository;OCIRepository;Bucket
	// +required
	Kind string `json:"kind"`

	// Name of the referenced source.
	// +required
	Name string `json:"name"`

	// Namespace of the referenced source. Must be the consuming object's own namespace, which is
	// also the default — the field remains only so an explicit value is not a schema error.
	//
	// A source elsewhere is REFUSED. Both controllers read Flux sources cluster-wide, so honouring
	// another namespace would let anyone who can create one of these objects pull that namespace's
	// content into an image they control and can read.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Revision the artifact is expected to be at. Optional; unset consumes whatever the source
	// currently publishes.
	//
	// This is the only way to make a sourceRef layer a pure function of the spec. Without it the
	// source can move under a fixed spec — a branch or a semver range does so with no edit to
	// anything, so nothing observes it. It is also independent of the source controller's own
	// bookkeeping: the staleness check compares generation against observedGeneration, which is the
	// source reporting on itself, whereas this is an assertion from the consuming side.
	//
	// Matched against Flux's "<ref>@<algo>:<hash>" by whichever half you give:
	//
	//	revision: v0.6.8                  matches v0.6.8@sha1:<anything>
	//	revision: v0.6.8@sha1:b739efb5    matches only that commit
	//
	// The short form exists because a generator usually knows the tag it asked for and not the
	// commit it resolved to, and the useful check should not require the half it cannot supply.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Revision string `json:"revision,omitempty"`

	// Subpath selects one directory from the artifact. Defaults to the whole thing.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Subpath string `json:"subpath,omitempty"`
}

// RevisionMatches reports whether an artifact's revision satisfies what the spec asked for.
//
// Flux revisions are "<ref>@<algo>:<hash>". A want with no "@" is compared against the ref half
// only, so pinning a tag does not require knowing the commit it resolved to.
func RevisionMatches(want, got string) bool {
	if want == "" {
		return true
	}
	if want == got {
		return true
	}
	if strings.Contains(want, "@") {
		return false
	}
	ref, _, found := strings.Cut(got, "@")
	return found && ref == want
}
