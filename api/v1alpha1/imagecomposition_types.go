package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Unpack describes how a fetched blob is turned into layer content.
// +kubebuilder:validation:Enum=none;tar;tar.gz;tar.xz;tar.zst;tar.bz2;gz;zip;deb
type Unpack string

const (
	// UnpackNone places the fetched bytes as a single file at the target.
	UnpackNone Unpack = "none"
	// UnpackTar extracts a tar archive under the target.
	UnpackTar Unpack = "tar"
	// UnpackTarGz extracts a gzipped tar archive under the target.
	UnpackTarGz Unpack = "tar.gz"
	// UnpackTarXz extracts an xz-compressed tar archive under the target.
	UnpackTarXz Unpack = "tar.xz"
	// UnpackTarZstd extracts a zstd-compressed tar archive under the target.
	UnpackTarZstd Unpack = "tar.zst"
	// UnpackTarBz2 extracts a bzip2-compressed tar archive under the target.
	UnpackTarBz2 Unpack = "tar.bz2"
	// UnpackGz decompresses a single gzipped file to the target.
	//
	// Not an archive: "to" must name a file and subpath is invalid. The file name comes from "to"
	// alone, never the URL or the gzip header. For a .tar.gz use tar.gz.
	UnpackGz Unpack = "gz"
	// UnpackZip extracts a zip archive under the target.
	//
	// A zip made on Windows records no unix permissions, so every file lands non-executable and a
	// binary needs mode: {file: "0755"}. Non-UTF-8 names, duplicate names and encrypted entries
	// are refused.
	UnpackZip Unpack = "zip"
	// UnpackDeb extracts a Debian package's data member under the target.
	//
	// Nothing is installed: no dependency is resolved and no maintainer script runs. See ADR 0022.
	UnpackDeb Unpack = "deb"
)

// BaseImage is the image the artifact is built on top of.
//
// Its layers are reused verbatim underneath every entry in spec.layers (ADR 0016). Omit it for a
// scratch artifact, the common case for something that is only ever mounted.
//
// Name it as one `ref` or as the older `image` + `digest` pair. `ref` is the form tools like
// Renovate and kustomize's `images` transformer can update.
//
// +kubebuilder:validation:XValidation:rule="(has(self.ref)?1:0) + (has(self.image)?1:0) == 1",message="set exactly one of ref or image"
// +kubebuilder:validation:XValidation:rule="has(self.image) == has(self.digest)",message="image and digest go together; ref is the combined form"
type BaseImage struct {
	// Ref names the base in one string: "quay.io/strimzi/kafka:0.43.0@sha256:…".
	//
	// The digest is mandatory and is what gets pulled. The tag is for humans and ignored when
	// pulling (ADR 0002).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*(:[a-zA-Z0-9._-]+)?@sha256:[a-f0-9]{64}$`
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Ref string `json:"ref,omitempty"`

	// Image is the repository to pull from, e.g. "quay.io/strimzi/kafka". Use with Digest, or use
	// Ref instead.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Image string `json:"image,omitempty"`

	// Digest pins the exact content. Required alongside Image, like every other input.
	//
	// With spec.platforms unset it must name a platform-specific manifest, not a
	// multi-architecture index; with spec.platforms set, an index is correct. See ADR 0015 and 0018.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`

	// SecretRef names a kubernetes.io/dockerconfigjson Secret for pulling, when the base is
	// private, matched by registry host like a Pod's imagePullSecrets. Separate from
	// spec.push.secretRef.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
}

// FetchSource retrieves content over HTTP(S).
//
// +kubebuilder:validation:XValidation:rule="!has(self.stripComponents) || self.stripComponents == 0 || (has(self.unpack) && self.unpack != 'none' && self.unpack != 'gz')",message="stripComponents applies to archive unpack modes only: 'none' and 'gz' place a single file, so there are no path components to remove"
type FetchSource struct {
	// URL to fetch.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=2048
	// +required
	URL string `json:"url"`

	// Digest of the fetched bytes. A mismatch is terminal. See ADR 0002.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +required
	Digest string `json:"digest"`

	// Unpack controls how the bytes become layer content.
	// +kubebuilder:default="none"
	// +optional
	Unpack Unpack `json:"unpack,omitempty"`

	// Subpath selects one directory from inside the archive and places its contents at the target.
	// A subpath matching nothing is an error. Archive unpack modes only: invalid with "gz",
	// ignored with "none".
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Subpath string `json:"subpath,omitempty"`

	// StripComponents removes this many leading path components from every entry, before Subpath
	// is applied.
	//
	// For "app-1.2.3/Dockerfile", `stripComponents: 1` drops the versioned directory, unlike
	// `subpath: app-1.2.3`, which must be edited every release. Same for every archive format.
	// Zero, the default, leaves paths alone. Removing every entry is an error.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=16
	// +optional
	StripComponents int `json:"stripComponents,omitempty"`
}

// ConfigMapSource turns a ConfigMap's entries into files.
//
// Each key becomes one file directly under the target. ConfigMap keys cannot contain "/", so
// nested directory layouts are not expressible this way — use a sourceRef for those.
//
// The content is hashed, so an edit rebuilds promptly.
type ConfigMapSource struct {
	// Name of the ConfigMap, in the ImageComposition's namespace.
	// +required
	Name string `json:"name"`

	// Optional tolerates the ConfigMap not existing, contributing no layer at all instead of
	// stalling.
	// +kubebuilder:default=false
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// Repository returns the registry reference to pull from, with any tag stripped, and the digest
// that pins it. It accepts either spelling; CEL has already ensured exactly one is set.
//
// The tag is dropped: pulling by digest alone means a moved tag cannot change what is pulled.
func (b *BaseImage) Repository() (repository, digest string) {
	if b.Ref != "" {
		return splitPinnedRef(b.Ref)
	}
	return b.Image, b.Digest
}

// Repository returns the reference and digest for an image layer source.
func (i *ImageSource) Repository() (repository, digest string) {
	return splitPinnedRef(i.Ref)
}

// splitPinnedRef separates "repo:tag@sha256:…" into its repository and digest.
//
// Both callers' fields are pattern-validated to carry "@sha256:", so this needs no error.
func splitPinnedRef(ref string) (repository, digest string) {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return ref, ""
	}
	repository, digest = ref[:at], ref[at+1:]

	// A tag follows the last ":" after the last "/", so a registry port is not mistaken for one.
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}
	return repository, digest
}

// ImageSource takes the flattened filesystem of a digest-pinned image.
//
// The image's layers are flattened (whiteouts applied, later layers overlaying earlier ones) into
// EXACTLY ONE layer at the target (ADR 0016), so `subpath`, `to`, `owner` and `mode` mean what they
// mean everywhere else. Flattening gives up blob sharing with the source image, so use spec.base
// to build on top of an image (ADR 0015).
type ImageSource struct {
	// Ref is the image to read, pinned by digest: "repo:tag@sha256:…" or "repo@sha256:…".
	//
	// The tag is for humans and ignored when pulling, as for spec.base.ref (ADR 0002).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*(:[a-zA-Z0-9._-]+)?@sha256:[a-f0-9]{64}$`
	// +kubebuilder:validation:MaxLength=1024
	// +required
	Ref string `json:"ref"`

	// Subpath selects one directory from the flattened filesystem and strips the prefix, so
	// "/usr/local/bin" from the image can land directly at the target.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Subpath string `json:"subpath,omitempty"`

	// SecretRef names a kubernetes.io/dockerconfigjson Secret for pulling, when the image is
	// private. Same arrangement as spec.base.secretRef.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
}

// Ownership sets uid and gid on the files a layer contributes.
//
// Defaults to 0:0, which suits content the workload only reads; set this when a process must own
// what it reads.
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
// executable, 0644 otherwise, so upstream permissions cannot vary the output digest.
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
// Each entry produces exactly one layer. Exactly one verb must be set; source-specific options live
// inside their verb.
//
// +kubebuilder:validation:XValidation:rule="(has(self.fetch)?1:0) + (has(self.configMap)?1:0) + (has(self.sourceRef)?1:0) + (has(self.image)?1:0) + (has(self.remove)?1:0) == 1",message="set exactly one of fetch, configMap, sourceRef, image or remove"
// +kubebuilder:validation:XValidation:rule="has(self.remove) ? (!has(self.to) && !has(self.owner) && !has(self.mode)) : has(self.to)",message="'to' is required for content entries and must be omitted for remove, which takes absolute paths; owner and mode do not apply to remove"
type Layer struct {
	// Name identifies this entry, and appears in messages and provenance.
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// Fetch retrieves content over HTTP(S).
	// +optional
	Fetch *FetchSource `json:"fetch,omitempty"`

	// Image takes the filesystem of another image as this entry's content, placed at a path like
	// any other content.
	// +optional
	Image *ImageSource `json:"image,omitempty"`

	// ConfigMap turns a ConfigMap's entries into files.
	// +optional
	ConfigMap *ConfigMapSource `json:"configMap,omitempty"`

	// SourceRef takes content from a Flux source's artifact.
	// +optional
	SourceRef *SourceRefSource `json:"sourceRef,omitempty"`

	// Remove deletes paths inherited from the base or from earlier layers.
	//
	// Implemented as OCI whiteout entries: the bytes remain in the layer below, so this hides a
	// file rather than reclaiming its space. Paths are absolute and refer to the assembled
	// filesystem.
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
// Everything here is a pure function of the spec. See ADR 0016.
type ImageConfig struct {
	// Inherit takes the base image's config as the starting point: its entrypoint, env, user,
	// working directory, exposed ports and stop signal. Without it the config starts empty.
	//
	// Fields set below override what is inherited.
	// +kubebuilder:default=false
	// +optional
	Inherit bool `json:"inherit,omitempty"`

	// Labels to set on the image config.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Env to set, as "KEY=value" entries. Replaces the inherited list rather than merging into it.
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
// It cannot execute anything: everything here is a pure function of its inputs, which makes the
// output digest predictable and the provenance exact. Anything needing a compiler belongs in CI.
// See ADR 0001 and ADR 0016.
// +kubebuilder:validation:XValidation:rule="!has(self.config) || !self.config.inherit || has(self.base)",message="config.inherit requires a base to inherit from"
type ImageCompositionSpec struct {
	// Interval at which to reconcile. Reconciling is nearly free when nothing has changed: the
	// controller compares a hash of the inputs rather than rebuilding.
	//
	// A pointer: as a value struct it would serialise as "0s" and the default would never apply.
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
	// base may then be a multi-architecture index. With one entry, or none, the output is a single
	// image manifest.
	//
	// Unset resolves to the BASE's platform, or with no base the CONTROLLER's own architecture
	// (ADR 0002). On a MIXED-architecture cluster, name the platforms here or pin the controller
	// to one architecture, or the output depends on where the controller runs.
	//
	// Naming a platform the base index does not contain is an error.
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

	// Push is where the artifact goes: tags, retention, and the tag-conflict policy.
	//
	// Optional. Omitted, the artifact publishes to the operator's default registry as
	// <namespace>/<name>. Set `repository` to publish elsewhere, with `secretRef`: the operator's
	// credential is only sent to the operator's own registry (ADR 0034).
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

	// Attestations records what supply-chain material is attached to Artifact, so a converged
	// reconcile can tell there is nothing to do without asking the registry.
	// +optional
	Attestations *AttestationStatus `json:"attestations,omitempty"`

	// History records past builds, newest first, capped at the retention count. Their images are
	// kept alive while listed. See ADR 0011 and ADR 0031.
	// +optional
	History []BuildRecord `json:"history,omitempty"`

	// Conflict records content this object produced and did not publish, because onConflict: Keep
	// left an existing tag in place. Cleared as soon as a reconcile publishes cleanly.
	// +optional
	Conflict *TagConflictStatus `json:"conflict,omitempty"`

	// LastHandledReconcileAt echoes the reconcile.fluxcd.io/requestedAt annotation, so
	// `flux reconcile` works out of the box.
	// +optional
	LastHandledReconcileAt string `json:"lastHandledReconcileAt,omitempty"`

	// RefExport is the ConfigMap push.writeRefTo last wrote, so the controller can clean it up
	// when the spec moves it elsewhere or stops asking for it.
	// +optional
	RefExport *RefExportStatus `json:"refExport,omitempty"`
}

// The keep annotation stops Helm deleting the CRD, and so every object of this kind, on uninstall.
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=imgcomp
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=`.status.artifact.ref`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ImageComposition assembles an OCI artifact from content-addressed inputs.
type ImageComposition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   ImageCompositionSpec   `json:"spec"`
	Status ImageCompositionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ImageCompositionList contains a list of ImageComposition.
type ImageCompositionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageComposition `json:"items"`
}

// These types are registered by addKnownTypes in groupversion_info.go.
