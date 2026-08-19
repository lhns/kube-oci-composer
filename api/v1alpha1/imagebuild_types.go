package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImageBuildSpec builds an OCI image by executing a Dockerfile.
//
// This kind executes arbitrary code, so its output digest is NOT a function of its spec — it is an
// observation, recorded in status after the fact. What the API can offer is that the INPUTS are
// content-addressed, so an unchanged input hash skips the build. Two clusters applying the same
// commit can still produce two different images.
//
// If what you need is "take a released artifact and put it in an image", use ImageComposition — it
// is a strictly stronger tool, and since ADR 0024 it can take files out of an image your CI already
// built. See ADR 0025 for what this kind costs.
type ImageBuildSpec struct {
	// Interval at which to reconcile. Nearly free when nothing has changed: the controller
	// compares a hash of the resolved inputs rather than building.
	//
	// It never rebuilds on a timer: a new digest under an unchanged spec is what immutable tags
	// refuse. To pick up upstream fixes, change an input — repin FROM, or move the context.
	// +kubebuilder:default="1h"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Suspend stops reconciling this object without deleting it.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Context is the build context, taken from a Flux source's artifact.
	//
	// From a Flux source, so the revision is content-addressed and its digest is what makes the
	// input hash meaningful. There is no inline or URL form: an unaddressed context would leave
	// nothing to hash, and every reconcile would be a build.
	// +required
	Context SourceRefSource `json:"context"`

	// Dockerfile is the path to the Dockerfile within the context.
	// +kubebuilder:default="Dockerfile"
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Dockerfile string `json:"dockerfile,omitempty"`

	// Target selects a stage in a multi-stage build. Empty builds the last stage.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Target string `json:"target,omitempty"`

	// Platforms the image is built for, as "linux/amd64".
	//
	// Required, unlike ImageComposition's — there is no base in the spec to default from.
	//
	// More than one entry produces an image index and needs a builder that can emulate or a
	// multi-node builder. A platform the builder cannot produce fails the build rather than being
	// silently dropped.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]+/[a-z0-9]+(/[a-z0-9]+)?$`
	// +required
	Platforms []string `json:"platforms"`

	// Args are Dockerfile ARG values.
	//
	// They are part of the input hash, and they are PLAINTEXT in the spec, in etcd and in
	// `kubectl get -o yaml`. Never put a credential here; use secrets, which are mounted for the
	// duration of one RUN and never land in a layer.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Args []BuildArg `json:"args,omitempty"`

	// Secrets are mounted via BuildKit's secret mount, so their content never lands in a layer —
	// the only safe way to use a credential in a build.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Secrets []BuildSecret `json:"secrets,omitempty"`

	// Network controls whether RUN can reach the network.
	//
	// "None" is the only mode in which this kind approaches ImageComposition's guarantee, and it is
	// unusable for any Dockerfile that installs packages — which is most of them. "Sandbox" is the
	// default precisely because of that, and it is where reproducibility is lost.
	// +kubebuilder:validation:Enum=Sandbox;None
	// +kubebuilder:default="Sandbox"
	// +optional
	Network string `json:"network,omitempty"`

	// Cache controls the build cache.
	// +optional
	Cache *BuildCache `json:"cache,omitempty"`

	// Resources for the build pod. The namespace's ResourceQuota and LimitRange apply on top,
	// which is deliberate: a build is a workload, and the cluster already knows how to govern one.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Timeout after which the build pod is deleted and the attempt recorded as failed.
	// +kubebuilder:default="30m"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// ServiceAccountName the build pod runs as. Empty uses the namespace's default account with
	// NO API token mounted, which is what a pod running code from a git repository should have.
	//
	// Set this only when a build genuinely needs an identity — pulling from a registry that
	// authenticates by workload identity, say. Naming an account mounts its token, so whatever it
	// can do, a Dockerfile in the referenced repository can do.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// Push publishes the built image to an external registry.
	//
	// Required in this alpha: the build runs in a Job, which cannot reach the controller's
	// loopback-only serving endpoint. See ADR 0025.
	// +required
	Push *Push `json:"push"`
}

// BuildArg is one Dockerfile ARG value.
type BuildArg struct {
	// Name of the ARG.
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// Value to pass. Plaintext; see the warning on Args.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Value string `json:"value,omitempty"`
}

// BuildSecret is a credential made available to one RUN via BuildKit's secret mount.
type BuildSecret struct {
	// ID is what `RUN --mount=type=secret,id=<ID>` refers to.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	// +kubebuilder:validation:MaxLength=253
	// +required
	ID string `json:"id"`

	// SecretRef names a Secret in this object's namespace.
	//
	// Cross-namespace is not offered: it would let anyone who can create an ImageBuild read any
	// Secret in the cluster.
	// +required
	SecretRef *LocalObjectReference `json:"secretRef"`

	// Key within the Secret. Defaults to the ID.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Key string `json:"key,omitempty"`
}

// BuildCache controls the build cache.
type BuildCache struct {
	// Mode selects whether a cache is used at all. "Disabled" is how you demonstrate that a
	// rebuild reproduces the previous digest; it is not how you should run day to day.
	// +kubebuilder:validation:Enum=Auto;Disabled
	// +kubebuilder:default="Auto"
	// +optional
	Mode string `json:"mode,omitempty"`

	// Ref is where the cache is exported to and imported from. Defaults to a per-object ref
	// derived from push.repository.
	//
	// A cache shared between objects is a channel between whoever can write their Dockerfiles, so
	// this is never defaulted to anything shared.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Ref string `json:"ref,omitempty"`
}

// BuildAttempt records one execution, successful or not.
type BuildAttempt struct {
	// InputHash the attempt was made for.
	// +optional
	InputHash string `json:"inputHash,omitempty"`

	// PodName of the build pod. Logs are not copied into status, so this is what `kubectl logs`
	// needs.
	// +optional
	PodName string `json:"podName,omitempty"`

	// StartedAt and FinishedAt bound the attempt.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Succeeded records the outcome.
	// +optional
	Succeeded bool `json:"succeeded,omitempty"`

	// Message is the failure reason, truncated.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Message string `json:"message,omitempty"`
}

// ImageBuildStatus is the observed state.
type ImageBuildStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// InputHash summarises everything that determines the build.
	//
	// Unlike ImageComposition's field of the same name, this hash is the IDENTITY rather than a
	// short-circuit: there is nothing to check it against until a build has run. See ADR 0025.
	// +optional
	InputHash string `json:"inputHash,omitempty"`

	// Conditions follow the same kstatus conventions as ImageComposition.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Artifact is what was produced. Deliberately the same shape every kind in this group uses.
	// +optional
	Artifact *ArtifactStatus `json:"artifact,omitempty"`

	// History is the retained builds, newest first.
	// +optional
	History []BuildRecord `json:"history,omitempty"`

	// BuildRef is the Job currently executing, so a controller that restarts mid-build adopts it
	// rather than starting a second one.
	// +optional
	BuildRef *LocalObjectReference `json:"buildRef,omitempty"`

	// LastAttempt records the most recent execution.
	// +optional
	LastAttempt *BuildAttempt `json:"lastAttempt,omitempty"`

	// Failures counts consecutive failed attempts, so backoff can be capped and the object can stop
	// hammering without being Stalled — the fix for a failing RUN lives in another object.
	// +optional
	Failures int32 `json:"failures,omitempty"`

	// LastHandledReconcileAt echoes the reconcile-request annotation once acted on.
	// +optional
	LastHandledReconcileAt string `json:"lastHandledReconcileAt,omitempty"`
}

// ImageBuild builds an OCI image from a Dockerfile and a content-addressed context.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ibuild
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=`.status.artifact.ref`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ImageBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ImageBuildSpec `json:"spec"`
	// +optional
	Status ImageBuildStatus `json:"status,omitempty"`
}

// ImageBuildList is a list of ImageBuild.
// +kubebuilder:object:root=true
type ImageBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageBuild `json:"items"`
}
