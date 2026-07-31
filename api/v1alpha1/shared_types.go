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
	// digest mismatch, a refusal to overwrite immutable content. Anything that a human must
	// change belongs here rather than in a hot retry loop.
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
)

// Finalizer is set on objects so published artifacts can be cleaned up on delete.
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

	// Tag is a MOVING POINTER, repointed at the newest build. It exists for image automation
	// to watch and is NOT intended to be referenced by a workload.
	//
	// Every build also publishes an immutable content tag, "<tag>-<digest[:12]>", which is
	// never reused for different content. Workloads should reference a digest — either written
	// by Flux image-automation, or the immutable content tag pinned by hand. See ADR 0010.
	// +kubebuilder:default="latest"
	// +optional
	Tag string `json:"tag,omitempty"`
}

// Push describes an external registry to publish to. Optional: omit it to use the built-in
// serving endpoint instead.
type Push struct {
	// Repository is the fully qualified target, e.g. "ghcr.io/example/artifact".
	// +required
	Repository string `json:"repository"`

	// Tag is the moving pointer, as described on Publish.Tag.
	// +kubebuilder:default="latest"
	// +optional
	Tag string `json:"tag,omitempty"`

	// SecretRef names a docker-registry Secret holding push credentials.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// Immutable refuses to overwrite the moving pointer when it already resolves to different
	// content. Defaults to false because the pointer is *meant* to move; set it true when the
	// tag is a version that must never change meaning.
	// +kubebuilder:default=false
	// +optional
	Immutable bool `json:"immutable,omitempty"`
}

// ArtifactStatus records what was produced. Deliberately identical in shape across every kind
// in this API group so consumers need not care which controller produced it.
type ArtifactStatus struct {
	// Digest of the published manifest, e.g. "sha256:...". This is the value to pin.
	// +optional
	Digest string `json:"digest,omitempty"`

	// Revision is the human-facing "<tag>@<digest>".
	// +optional
	Revision string `json:"revision,omitempty"`

	// Ref is the complete pullable reference, including the serving host in serving mode.
	// Surfaced as a printer column so `kubectl get` shows the exact string to use.
	// +optional
	Ref string `json:"ref,omitempty"`

	// ContentTag is the immutable "<tag>-<digest[:12]>" reference for this exact content. It is
	// never reused, so pinning it by hand is safe when image automation is not in use.
	// +optional
	ContentTag string `json:"contentTag,omitempty"`

	// LastUpdateTime is when this artifact was last published.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`
}
