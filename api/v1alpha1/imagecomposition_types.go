package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Unpack describes how a fetched blob is turned into layer content.
// +kubebuilder:validation:Enum=none;tar;tar.gz
type Unpack string

const (
	// UnpackNone places the fetched bytes as a single file at Target.
	UnpackNone Unpack = "none"
	// UnpackTar extracts a tar archive under Target.
	UnpackTar Unpack = "tar"
	// UnpackTarGz extracts a gzipped tar archive under Target.
	UnpackTarGz Unpack = "tar.gz"
)

// URLSource fetches a blob over HTTP(S).
type URLSource struct {
	// URL to fetch.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +required
	URL string `json:"url"`
}

// ImageSource contributes the layers of an existing image. A base image is not a special
// field — it is simply the first entry that happens to be an image. See ADR 0003.
type ImageSource struct {
	// Repository of the image, e.g. "gcr.io/distroless/static".
	// +required
	Repository string `json:"repository"`

	// SecretRef names a docker-registry Secret for pulling, when the image is private.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
}

// Layer is one entry in an ordered list of content contributions.
//
// Exactly one source must be set. This is modelled as a discriminated union from day one so
// that later source kinds (sourceRef to a Flux source, imageRef to another object in this API
// group) can be added without a breaking schema change. See ADR 0004.
//
// +kubebuilder:validation:XValidation:rule="[has(self.url), has(self.image)].filter(x, x).size() == 1",message="exactly one of url or image must be set"
type Layer struct {
	// Name identifies this entry. Used in messages, in provenance attestations, and as the
	// target of config.from.
	// +required
	Name string `json:"name"`

	// URL fetches content over HTTP(S).
	// +optional
	*URLSource `json:",inline"`

	// Image contributes the layers of an existing image.
	// +optional
	Image *ImageSource `json:"image,omitempty"`

	// Digest of the fetched content. REQUIRED, with no exceptions anywhere in this API — it is
	// what makes the output digest a pure function of the spec, and it makes tampering
	// detectable rather than silent. A mismatch is terminal (Stalled), never a retry.
	// See ADR 0002.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +required
	Digest string `json:"digest"`

	// Unpack controls how the fetched bytes become layer content.
	// +kubebuilder:default="none"
	// +optional
	Unpack Unpack `json:"unpack,omitempty"`

	// Target is the absolute path inside the image the content is placed at.
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:default="/"
	// +optional
	Target string `json:"target,omitempty"`
}

// ImageConfig is the OCI config for the produced image. Explicit rather than silently
// inherited from whichever entry happens to be an image, so the result is unambiguous.
type ImageConfig struct {
	// From names a layer entry to inherit the OCI config from. Without it the config is empty,
	// which is correct for artifacts that are only ever mounted rather than executed.
	// +optional
	From string `json:"from,omitempty"`

	// Labels to set on the image config.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Env to set on the image config.
	// +optional
	Env []string `json:"env,omitempty"`

	// Entrypoint to set on the image config.
	// +optional
	Entrypoint []string `json:"entrypoint,omitempty"`

	// Cmd to set on the image config.
	// +optional
	Cmd []string `json:"cmd,omitempty"`
}

// ImageCompositionSpec assembles an OCI image from content-addressed inputs.
//
// It deliberately cannot run a Dockerfile. Composition is a pure function of its inputs, which
// is what allows the reconcile loop to be idempotent and convergent and what allows provenance
// to be exact rather than scanned. Anything requiring arbitrary execution belongs to a
// different kind with a weaker guarantee. See ADR 0001.
//
// +kubebuilder:validation:XValidation:rule="!has(self.push) || !has(self.publish)",message="set at most one of push or publish"
type ImageCompositionSpec struct {
	// Interval at which to reconcile. Reconciling is nearly free when nothing has changed: the
	// controller computes the expected digest and compares, rather than rebuilding.
	// +kubebuilder:default="1h"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// Suspend halts reconciliation without deleting anything already published.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Layers is an ordered, non-empty list of content contributions. Later entries overlay
	// earlier ones. There is no separate base field: a base image is just the first entry.
	// +kubebuilder:validation:MinItems=1
	// +required
	Layers []Layer `json:"layers"`

	// Config for the produced image.
	// +optional
	Config *ImageConfig `json:"config,omitempty"`

	// Publish serves the artifact from the controller's own read-only OCI endpoint. This is the
	// default mode and needs no registry and no credentials. Defaulted from the object's name
	// when neither publish nor push is set.
	// +optional
	Publish *Publish `json:"publish,omitempty"`

	// Push publishes to an external registry instead. Use it when the artifact must outlive the
	// cluster, be consumed from outside it, or carry registry-native attestations.
	// +optional
	Push *Push `json:"push,omitempty"`
}

// ImageCompositionStatus reports what was produced.
type ImageCompositionStatus struct {
	// ObservedGeneration is the last spec generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// InputHash summarises everything that determines the output: the ordered layer digests,
	// their unpack modes and targets, the config, and the assembly algorithm version. When it is
	// unchanged and the published artifact still resolves, the controller skips the whole build
	// — no fetch, no assembly, one HEAD.
	//
	// This is NOT the same idea as ImageBuild's planned inputHash. There the hash IS the
	// identity, because a Dockerfile's output digest cannot be known without building. Here the
	// output digest remains the identity and this is only a short-circuit; the guarantee is
	// unchanged, the work is skipped. See ADR 0002.
	// +optional
	InputHash string `json:"inputHash,omitempty"`

	// Conditions follow kstatus: Ready, Reconciling, Stalled.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Artifact describes the published result.
	// +optional
	Artifact *ArtifactStatus `json:"artifact,omitempty"`

	// LastHandledReconcileAt echoes the reconcile.fluxcd.io/requestedAt annotation, so
	// `flux reconcile` works out of the box.
	// +optional
	LastHandledReconcileAt string `json:"lastHandledReconcileAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=imgcomp
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=`.status.artifact.ref`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ImageComposition assembles an OCI image from content-addressed inputs.
type ImageComposition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageCompositionSpec   `json:"spec,omitempty"`
	Status ImageCompositionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ImageCompositionList contains a list of ImageComposition.
type ImageCompositionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageComposition `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageComposition{}, &ImageCompositionList{})
}
