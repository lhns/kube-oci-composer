package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Unpack describes how a fetched blob is turned into layer content.
// +kubebuilder:validation:Enum=none;tar;tar.gz
type Unpack string

const (
	// UnpackNone places the fetched bytes as a single file at the target.
	UnpackNone Unpack = "none"
	// UnpackTar extracts a tar archive under the target.
	UnpackTar Unpack = "tar"
	// UnpackTarGz extracts a gzipped tar archive under the target.
	UnpackTarGz Unpack = "tar.gz"
)

// BaseImage is the image the artifact is built on top of.
//
// Hoisted out of the layer list rather than being one entry among many, which reverses the
// original design. Three things made that untenable: the config had to name which entry was the
// base, an image entry contributes many layers where every other entry contributes one, and
// multi-architecture builds resolve only the base per platform. Three exceptions is enough to
// conclude it was never an ordinary entry. See ADR 0016.
//
// Omit it entirely for a scratch artifact — a bundle of files with no base is the common case for
// something that is only ever mounted.
type BaseImage struct {
	// Image is the repository to pull from, e.g. "quay.io/strimzi/kafka".
	// +kubebuilder:validation:MaxLength=512
	// +required
	Image string `json:"image"`

	// Digest pins the exact content. Required, like every other input.
	//
	// It must name a platform-specific manifest, not a multi-architecture index: resolving an
	// index would mean the controller choosing a platform, so the output would stop being a
	// function of the spec. Multi-architecture output is not implemented; when it is, an index
	// here becomes correct because the platform list will come from the spec. See ADR 0015.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +required
	Digest string `json:"digest"`

	// SecretRef names a kubernetes.io/dockerconfigjson Secret for pulling, when the base is
	// private. The registry host comes from the Secret's auths map, matched against Image — the
	// same arrangement as a Pod's imagePullSecrets, so no separate registry object is needed.
	//
	// Deliberately not shared with spec.push.secretRef: different registry, different scope, and
	// sending a push-scoped token to wherever the base lives is not a reasonable default.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
}

// FetchSource retrieves content over HTTP(S).
type FetchSource struct {
	// URL to fetch.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=2048
	// +required
	URL string `json:"url"`

	// Digest of the fetched bytes. Declared here rather than resolved, because an arbitrary URL
	// is not content-addressed by anything else. A mismatch is terminal. See ADR 0002.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +required
	Digest string `json:"digest"`

	// Unpack controls how the bytes become layer content.
	// +kubebuilder:default="none"
	// +optional
	Unpack Unpack `json:"unpack,omitempty"`

	// Subpath selects one directory from inside the archive and strips the prefix. Useful when a
	// release tarball wraps everything in a version-named directory you do not want in the image.
	// A subpath matching nothing is an error rather than a silently empty layer.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Subpath string `json:"subpath,omitempty"`
}

// ConfigMapSource turns a ConfigMap's entries into files.
//
// Each key becomes one file directly under the target. ConfigMap keys cannot contain "/", so
// nested directory layouts are not expressible this way — use a sourceRef for those.
//
// The digest is resolved by hashing the content, so an edit rebuilds. The ConfigMap is watched, so
// that happens promptly rather than at the next interval.
type ConfigMapSource struct {
	// Name of the ConfigMap, in the ImageComposition's namespace.
	// +required
	Name string `json:"name"`

	// Optional tolerates the ConfigMap not existing, contributing nothing instead of stalling.
	// Nothing, not an empty layer — an empty layer would still change the output digest.
	// +kubebuilder:default=false
	// +optional
	Optional bool `json:"optional,omitempty"`
}

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

	// Namespace of the referenced source. Defaults to the ImageComposition's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Subpath selects one directory from the artifact. Defaults to the whole thing.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Subpath string `json:"subpath,omitempty"`
}

// Ownership sets uid and gid on the files a layer contributes.
//
// Defaults to 0:0. Composed content is normally read by the workload rather than written, so
// root-owned and world-readable is usually right; set this when a process must own what it reads.
type Ownership struct {
	// UID to set on contributed files.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	UID int64 `json:"uid,omitempty"`

	// GID to set on contributed files.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	GID int64 `json:"gid,omitempty"`
}

// FileMode sets permissions on the files a layer contributes.
//
// Without it, modes are normalised: 0755 for directories and anything the source marked
// executable, 0644 otherwise. Normalisation exists so that whoever packed the upstream archive
// cannot vary the output digest through permissions nobody looks at.
type FileMode struct {
	// File mode for regular files, e.g. "0644".
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	// +optional
	File string `json:"file,omitempty"`

	// Dir mode for directories, e.g. "0755".
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	// +optional
	Dir string `json:"dir,omitempty"`
}

// Layer is one entry in an ordered list of filesystem operations applied on top of the base.
//
// Each entry produces exactly one layer, which is what makes the name accurate — the base is
// hoisted out precisely because it did not satisfy that.
//
// Exactly one verb must be set. Source-specific options live inside their verb rather than being
// smeared across the entry, so there is no field that is meaningful for one source and silently
// ignored by the rest.
//
// +kubebuilder:validation:XValidation:rule="(has(self.fetch)?1:0) + (has(self.configMap)?1:0) + (has(self.sourceRef)?1:0) + (has(self.remove)?1:0) == 1",message="set exactly one of fetch, configMap, sourceRef or remove"
// +kubebuilder:validation:XValidation:rule="has(self.remove) ? (!has(self.to) && !has(self.owner) && !has(self.mode)) : has(self.to)",message="'to' is required for content entries and must be omitted for remove, which takes absolute paths; owner and mode do not apply to remove"
type Layer struct {
	// Name identifies this entry, and appears in messages and provenance.
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// Fetch retrieves content over HTTP(S).
	// +optional
	Fetch *FetchSource `json:"fetch,omitempty"`

	// ConfigMap turns a ConfigMap's entries into files.
	// +optional
	ConfigMap *ConfigMapSource `json:"configMap,omitempty"`

	// SourceRef takes content from a Flux source's artifact.
	// +optional
	SourceRef *SourceRefSource `json:"sourceRef,omitempty"`

	// Remove deletes paths inherited from the base or from earlier layers.
	//
	// Implemented as whiteout entries, which is how OCI expresses deletion: the bytes remain in
	// the layer below, so this hides a file rather than reclaiming its space. Paths are absolute
	// and refer to the assembled filesystem.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=4096
	// +optional
	Remove []string `json:"remove,omitempty"`

	// To is the absolute path inside the image this entry's content is placed at.
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	To string `json:"to,omitempty"`

	// Owner sets uid and gid on the contributed files.
	// +optional
	Owner *Ownership `json:"owner,omitempty"`

	// Mode sets permissions on the contributed files.
	// +optional
	Mode *FileMode `json:"mode,omitempty"`
}

// ImageConfig is the OCI config stamped on the produced artifact.
//
// Everything here is a pure function of the spec, which is the only test for whether something
// belongs in this API at all. See ADR 0016.
type ImageConfig struct {
	// Inherit takes the base image's config as the starting point: its entrypoint, env, user,
	// working directory, exposed ports and stop signal. Without it the config starts empty.
	//
	// Opt-in rather than automatic. An artifact that is only ever mounted should have an empty
	// config, and silently acquiring a base's entrypoint would be surprising. Fields set below
	// override what is inherited.
	// +kubebuilder:default=false
	// +optional
	Inherit bool `json:"inherit,omitempty"`

	// Labels to set on the image config.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Env to set, as "KEY=value" entries. Replaces the inherited list rather than merging into
	// it: a per-key merge raises questions about ordering and removal with no obvious answers.
	// +optional
	Env []string `json:"env,omitempty"`

	// Entrypoint to set.
	// +optional
	Entrypoint []string `json:"entrypoint,omitempty"`

	// Cmd to set.
	// +optional
	Cmd []string `json:"cmd,omitempty"`

	// User to run as, e.g. "1001" or "1001:1001".
	// +kubebuilder:validation:MaxLength=256
	// +optional
	User string `json:"user,omitempty"`

	// WorkingDir to start in.
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	WorkingDir string `json:"workingDir,omitempty"`

	// ExposedPorts, e.g. "9092/tcp". Documentation for whoever reads the image; Kubernetes does
	// not consult it.
	// +optional
	ExposedPorts []string `json:"exposedPorts,omitempty"`

	// Volumes declares paths as volumes in the image config.
	// +optional
	Volumes []string `json:"volumes,omitempty"`

	// StopSignal, e.g. "SIGTERM".
	// +kubebuilder:validation:MaxLength=32
	// +optional
	StopSignal string `json:"stopSignal,omitempty"`
}

// ImageCompositionSpec assembles an OCI artifact from content-addressed inputs.
//
// It cannot execute anything, and that is the entire scope line. Everything expressible here is a
// pure function of its inputs, which is what makes the output digest predictable, the reconcile
// loop convergent, and the provenance exact rather than scanned. Anything needing a compiler
// belongs in ordinary CI. See ADR 0001 and ADR 0016.
//
// +kubebuilder:validation:XValidation:rule="!has(self.push) || !has(self.publish)",message="set at most one of push or publish"
// +kubebuilder:validation:XValidation:rule="!has(self.config) || !self.config.inherit || has(self.base)",message="config.inherit requires a base to inherit from"
type ImageCompositionSpec struct {
	// Interval at which to reconcile. Reconciling is nearly free when nothing has changed: the
	// controller compares a hash of the inputs rather than rebuilding.
	//
	// A POINTER, so that omitting it actually omits it. metav1.Duration is a struct, and
	// `omitempty` does not apply to structs — as a value it always serialised as "0s", which
	// meant the API server saw an explicit zero and the default below never applied.
	// +kubebuilder:default="1h"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Suspend halts reconciliation without deleting anything already published.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Base is the image to build on. Omit for a scratch artifact.
	// +optional
	Base *BaseImage `json:"base,omitempty"`

	// Platforms the artifact is built for, as "linux/amd64" or "linux/arm/v7".
	//
	// Two or more entries publish an OCI image INDEX with one child manifest per platform, and a
	// base may then be a multi-architecture index — its child is selected per platform. With one
	// entry, or none, the output is a single image manifest exactly as before.
	//
	// Unset resolves to one platform: the BASE's if there is a base, otherwise the CONTROLLER's
	// own. That second case is the only input to the output digest that does not come from the
	// spec (ADR 0002). It is the right default on a single-architecture cluster, where it is
	// precisely the value that used to be hardcoded. On a MIXED-architecture cluster it means the
	// same spec can produce different content depending on where the controller runs, which with
	// immutable tags surfaces as a failed build — name the platforms here, or pin the controller
	// to one architecture.
	//
	// Naming a platform the base index does not contain is an error: the spec asked for something
	// that does not exist, and a silent substitution would be worse.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]+/[a-z0-9]+(/[a-z0-9]+)?$`
	// +optional
	Platforms []string `json:"platforms,omitempty"`

	// Layers are ordered filesystem operations applied on top of the base. Later entries overlay
	// earlier ones, exactly as image layers do.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +required
	Layers []Layer `json:"layers"`

	// Config for the produced artifact.
	// +optional
	Config *ImageConfig `json:"config,omitempty"`

	// Publish serves the artifact from the controller's own read-only OCI endpoint. The default
	// mode, needing no registry and no credentials.
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

	// InputHash summarises everything that determines the output. When it is unchanged and the
	// published artifact still resolves, the controller skips the whole build — no fetch, no
	// assembly, one HEAD. See ADR 0002.
	// +optional
	InputHash string `json:"inputHash,omitempty"`

	// Conditions follow kstatus: Ready, Reconciling, Stalled.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Artifact describes the published result.
	// +optional
	Artifact *ArtifactStatus `json:"artifact,omitempty"`

	// History records past builds, newest first, capped at the retention count. It is the live
	// set garbage collection marks from. See ADR 0011.
	// +optional
	History []BuildRecord `json:"history,omitempty"`

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

// ImageComposition assembles an OCI artifact from content-addressed inputs.
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

// These types are registered by addKnownTypes in groupversion_info.go rather than an init() here,
// because apimachinery's SchemeBuilder takes functions where controller-runtime's took objects.
