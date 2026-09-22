package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types and reasons, following the kstatus conventions Flux uses, so `kubectl wait`,
// `flux get` and notification-controller behave as expected.
const (
	// ReadyCondition is the top-level summary condition.
	ReadyCondition = "Ready"
	// ReconcilingCondition signals work in progress; a transient failure keeps this set so the
	// object is retried with backoff.
	ReconcilingCondition = "Reconciling"
	// StalledCondition signals a TERMINAL error that retrying cannot fix: an invalid spec, a
	// digest mismatch, a refusal to overwrite immutable content.
	//
	// Only for failures fixed by editing THIS object's spec, whose generation change wakes the
	// controller. A failure fixed elsewhere (a Secret, a Flux source, operator configuration)
	// must not stall, since no such event would arrive. See ReasonDependencyNotReady.
	StalledCondition = "Stalled"
)

// Reasons attached to the conditions above.
const (
	ReasonSucceeded         = "Succeeded"
	ReasonProgressing       = "Progressing"
	ReasonDigestMismatch    = "DigestMismatch"
	ReasonInvalidSpec       = "InvalidSpec"
	ReasonImmutableConflict = "ImmutableTagConflict"
	ReasonFetchFailed       = "FetchFailed"
	ReasonAttestationFailed = "AttestationFailed"
	ReasonSuspended         = "Suspended"
	// ReasonRetentionDegraded reports that the refresh keeping this object's images from being
	// reclaimed keeps failing. It fails UNSAFE (ADR 0031): sustained failure ends in deletion.
	ReasonRetentionDegraded = "RetentionDegraded"

	// ReasonRetentionLost reports that a reference this object published is already gone from the
	// registry. Unlike RetentionDegraded (might be failing, can clear), this already happened and
	// cannot clear (ADR 0049).
	ReasonRetentionLost = "RetentionLost"

	// ReasonArtifactLost reports that an object's current artifact is gone from the registry. For an
	// ImageBuild a rebuild REPLACES it with a new digest, which does not help anything pinned to the
	// old one (ADR 0051).
	ReasonArtifactLost = "ArtifactLost"

	// ReasonUnpinnedSource reports a sourceRef with no revision feeding tags that cannot move: fixed
	// content promised from an input free to change under it (ADR 0052).
	ReasonUnpinnedSource = "UnpinnedSource"

	// ReasonBuildFailed covers an ImageBuild whose Job did not succeed. Never Stalled: the fix
	// lives in another object.
	ReasonBuildFailed = "BuildFailed"

	// ReasonDependencyNotReady covers something the object refers to that does not exist yet, or
	// cannot be used: a Flux source, a Secret, a non-optional ConfigMap. Never Stalled (see
	// StalledCondition): applying an object and its GitRepository together must not wedge it.
	ReasonDependencyNotReady = "DependencyNotReady"
)

// ReconcileRequestAnnotation is Flux's key, which `flux reconcile` writes. Controllers echo it into
// status.lastHandledReconcileAt once acted on.
const ReconcileRequestAnnotation = "reconcile.fluxcd.io/requestedAt"

// Finalizer is set on objects so published artifacts can be cleaned up on delete.
const Finalizer = "finalizers.oci.lhns.de"

// LocalObjectReference refers to an object in the same namespace. Credentials are always
// referenced, never inlined.
type LocalObjectReference struct {
	// Name of the referent.
	// +required
	Name string `json:"name"`
}

// ConfigMapKeyReference selects ONE entry of a ConfigMap in the same namespace. (ConfigMapSource,
// by contrast, turns every entry into a file.)
//
// There is no namespace field: naming another namespace would let anyone who can create the
// consuming object read that namespace's ConfigMaps (threat-model I4).
type ConfigMapKeyReference struct {
	// Name of the ConfigMap.
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// Key within it. Data is read first and BinaryData second.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:default="Dockerfile"
	// +optional
	Key string `json:"key,omitempty"`
}

// ResolveConflictPolicy returns the effective policy for a tag that already resolves to something
// else, reconciling the three-valued field with the deprecated two-valued one.
//
// Precedence is onConflict, then immutable, then Fail, so objects written before onConflict keep
// their `immutable`, and an explicit onConflict wins. CEL refuses contradictions. A nil Push is
// Fail, so tags are never unprotected by omission.
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

// HistoryLimit resolves how many past builds to retain: this object's own, else the operator's,
// else the built-in default.
//
// Shared by both kinds so they cannot drift. A non-positive value falls through.
func (p *Push) HistoryLimit(operator int) int {
	if p != nil && p.History != nil && *p.History > 0 {
		return int(*p.History)
	}
	if operator > 0 {
		return operator
	}
	return DefaultHistoryLimit
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

// DefaultHistoryLimit is how many past builds are retained when nothing says otherwise. Retaining
// one is cheap (layers are shared); reclaiming too eagerly breaks workloads pinned to a digest
// (ADR 0011).
const DefaultHistoryLimit = 10

// BuildRecord is one past build, retained in status so what must stay alive is an explicit record
// rather than inferred from storage (ADR 0031).
type BuildRecord struct {
	// Tags this build was published under, if any. Replayed after a restart so the references a
	// workload names keep resolving; a build with no tags is still replayed by digest.
	// +optional
	Tags []string `json:"tags,omitempty"`

	// Digest of the manifest. For a multi-platform build this is the INDEX.
	// +optional
	Digest string `json:"digest,omitempty"`

	// Blobs are the config and layer digests this build is composed of. For a multi-platform
	// build it is the union across every child.
	// +optional
	Blobs []string `json:"blobs,omitempty"`

	// Sources records what each layer was resolved FROM, so an artifact can be traced back to the
	// revision that produced it (ADR 0026).
	// +optional
	Sources []SourceRecord `json:"sources,omitempty"`

	// InputHash the build was produced from. Written by ImageBuild only; ImageComposition's
	// identity is the output digest rather than the hash. See ADR 0025.
	// +optional
	InputHash string `json:"inputHash,omitempty"`

	// Manifests are the CHILD manifest digests when this build is a multi-platform index. Empty
	// for a single-platform build, where Digest is the manifest itself.
	// +optional
	Manifests []string `json:"manifests,omitempty"`

	// Time the build was published.
	// +optional
	Time *metav1.Time `json:"time,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!(has(self.immutable) && has(self.onConflict)) || (self.immutable && self.onConflict == 'Fail') || (!self.immutable && self.onConflict == 'Overwrite')",message="immutable and onConflict contradict each other: immutable true means onConflict Fail, immutable false means onConflict Overwrite. immutable is deprecated; prefer setting onConflict alone."
// Push describes where to publish. Optional: omitted, the object publishes to the operator's
// default registry.
type Push struct {
	// Repository is the fully qualified target, e.g. "ghcr.io/example/artifact".
	//
	// Optional. Omitted, the object publishes to the operator's default registry under
	// <namespace>/<name>, which a default chart install configures.
	//
	// The operator's registry credential is used ONLY for the default target. An object that names
	// its own repository authenticates with its own secretRef, or not at all.
	// +optional
	Repository string `json:"repository,omitempty"`

	// Tags are the tags to push, as described on Publish.Tags. Empty pushes by digest only.
	// Every publish is also tagged with its own digest, digest-<hex> (ADR 0060).
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9_][a-zA-Z0-9._-]*$`
	// +optional
	Tags []string `json:"tags,omitempty"`

	// SecretRef names a docker-registry Secret holding push credentials.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// Ref is a reference whose TAG is appended to Tags, so a kustomize images transformer or a
	// Helm value can retag without editing this list.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Ref string `json:"ref,omitempty"`

	// History is how many past builds to retain, overriding the controller's default.
	//
	// A composition can rebuild any artifact from its spec; an ImageBuild cannot (ADR 0025), so
	// there this is how much of the only copy is kept.
	// +kubebuilder:validation:Minimum=1
	// +optional
	History *int32 `json:"history,omitempty"`

	// Immutable is DEPRECATED; use OnConflict. true means onConflict: Fail, false means
	// onConflict: Overwrite.
	// +optional
	Immutable *bool `json:"immutable,omitempty"`

	// OnConflict decides what happens when a tag already resolves to different content: Fail
	// (refuse and stall), Overwrite (move the tag), or Keep (leave it, drop this build, stay
	// Ready). Defaults to Fail.
	//
	// Republishing IDENTICAL content is never a conflict. The check is against the digest actually
	// produced, on both kinds (ADR 0054), so a tag meant to MOVE needs Overwrite.
	//
	// No schema default: it would be applied to stored objects on read and silently turn every
	// `immutable: false` object into a refusing one. The effective default consults `immutable`.
	// +optional
	OnConflict TagConflictPolicy `json:"onConflict,omitempty"`

	// WriteRefTo exports the published reference into a ConfigMap. Off unless set. The target must
	// be the object's OWN namespace, or one the controller allow-lists. See RefExport.
	// +optional
	WriteRefTo *RefExport `json:"writeRefTo,omitempty"`
}

// TagConflictPolicy decides what happens when a tag already resolves to content other than what
// this spec produces.
//
// Keep exists for tags derived from a hash of the spec, where an existing tag means the content is
// already published and correct: refusing would stall over a non-problem, and overwriting would
// (for an ImageBuild) replace good content with a different build of the same inputs.
// +kubebuilder:validation:Enum=Fail;Overwrite;Keep
type TagConflictPolicy string

const (
	// ConflictFail refuses to change what a tag means, and stalls. The default: a silently moved
	// tag leaves nodes running different bytes under one name.
	ConflictFail TagConflictPolicy = "Fail"
	// ConflictOverwrite moves the tag. For a deliberately moving pointer, e.g. tags: [main].
	ConflictOverwrite TagConflictPolicy = "Overwrite"
	// ConflictKeep leaves the existing tag alone, drops what this reconcile produced, and reports
	// Ready. The dropped digest is recorded in status.conflict (see TagConflictStatus).
	ConflictKeep TagConflictPolicy = "Keep"
)

// TagConflictStatus records content this object produced and did NOT publish, because
// onConflict: Keep left an existing tag in place.
//
// It keeps Keep from becoming a silent divergence between what the spec produces and what the tag
// serves (ADR 0026). Cleared as soon as a reconcile publishes without conflict.
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
// It lets a converged reconcile skip asking the registry (ADR 0008). A new Subject invalidates it.
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

// ArtifactStatus records what was produced. The same shape for every kind in this API group.
type ArtifactStatus struct {
	// Digest of the published manifest, e.g. "sha256:...". This is the value to pin.
	// +optional
	Digest string `json:"digest,omitempty"`

	// Revision is the human-facing "<tag>@<digest>", using the first tag. Just the digest when
	// the build carries no tags.
	// +optional
	Revision string `json:"revision,omitempty"`

	// Ref is the complete pullable reference, as a workload should pull it. Shown by
	// `kubectl get`.
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

// Conditions accessors for the shared reconcile helpers, shaped like Flux's ObjectWithConditions.

func (o *ImageComposition) GetConditions() []metav1.Condition  { return o.Status.Conditions }
func (o *ImageComposition) SetConditions(c []metav1.Condition) { o.Status.Conditions = c }

func (o *ImageBuild) GetConditions() []metav1.Condition  { return o.Status.Conditions }
func (o *ImageBuild) SetConditions(c []metav1.Condition) { o.Status.Conditions = c }

// SourceRefSource takes content from a Flux source's artifact.
//
// The digest is resolved from the source's status.artifact. See ADR 0002.
type SourceRefSource struct {
	// Kind of the referenced source.
	// +kubebuilder:validation:Enum=GitRepository;OCIRepository;Bucket
	// +required
	Kind string `json:"kind"`

	// Name of the referenced source.
	// +required
	Name string `json:"name"`

	// Namespace of the referenced source. Must be the consuming object's own namespace, which is
	// also the default; any other namespace is REFUSED, since it would expose that namespace's
	// content to anyone who can create this object.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Revision the artifact is expected to be at. Optional; unset consumes whatever the source
	// currently publishes.
	//
	// This is the only way to make a sourceRef layer a pure function of the spec: without it, a
	// branch or semver range can move the source under an unchanged spec.
	//
	// Matched against Flux's "<ref>@<algo>:<hash>" by whichever half you give:
	//
	//	revision: v0.6.8                  matches v0.6.8@sha1:<anything>
	//	revision: v0.6.8@sha1:b739efb5    matches only that commit
	//
	// The short form suits a generator that knows the tag it asked for but not the commit.
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

// RefExport writes what was published into a ConfigMap, for a consumer that substitutes it.
//
// An ImageBuild's digest cannot be known in advance (ADR 0025), and an ImageComposition's is only
// computable by reproducing its spec hash (ADR 0017), so both kinds can export.
//
// THE CONFIGMAP'S NAME IS DERIVED, not chosen: <kind>-<namespace>-<object name>, e.g.
// imagebuild-team-a-app.
//
// Opt-in, and with a cost (ADR 0055): the digest becomes state outside git, so a revert no longer
// reverts the running image.
type RefExport struct {
	// Namespace to write the ConfigMap in.
	//
	// Must be the object's OWN namespace, or one the operator allow-listed with
	// --ref-export-namespaces (ADR 0056). Anything else is refused.
	//
	// No default: a substitution source is read from the CONSUMING Kustomization's namespace.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Keys names the ConfigMap keys to write.
	Keys RefExportKeys `json:"keys"`

	// Labels are added to the generated ConfigMap.
	//
	// Each key must be permitted by --ref-export-allowed-labels, which is empty by default; an
	// unpermitted key is refused rather than dropped. The controller's own labels are written last
	// and cannot be overridden.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations are added to the generated ConfigMap. Gated by
	// --ref-export-allowed-annotations, exactly as Labels is.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// RefExportKeys names what to write under which key. At least one is required.
//
// +kubebuilder:validation:XValidation:rule="has(self.ref) || has(self.digest)",message="set at least one of ref or digest"
type RefExportKeys struct {
	// Ref receives the full pullable reference, registry/repository@sha256:...
	//
	// Prefer this over Digest alone: a substituted bare digest fails less legibly if missing.
	// +optional
	Ref string `json:"ref,omitempty"`

	// Digest receives the bare sha256:... value.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// RefExportStatus records the ConfigMap this object last wrote.
//
// The namespace it was last written to is not derivable, so without this, moving or removing
// writeRefTo would strand the old ConfigMap (ADR 0056).
type RefExportStatus struct {
	// Name of the ConfigMap that was written.
	Name string `json:"name"`

	// Namespace it was written in.
	Namespace string `json:"namespace"`
}

// GetWriteRefTo is nil-safe, because spec.push may be omitted entirely.
func (p *Push) GetWriteRefTo() *RefExport {
	if p == nil {
		return nil
	}
	return p.WriteRefTo
}
