package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildContext is the tree the Dockerfile's COPY and ADD read from.
//
// Every member is content-addressed, and THAT is the requirement — not that it comes from Flux.
// This kind's input hash is its identity (ADR 0025), so a context nothing addresses would leave
// nothing to hash and every reconcile would be a build. A Flux artifact resolves to a digest; the
// members added later declare one. All of them satisfy the rule the earlier Flux-only shape was
// written to enforce, which is why that shape was narrower than its own reason. See ADR 0042.
//
// Still no inline or bare-URL form: those are the two that genuinely fail the test.
//
// +kubebuilder:validation:XValidation:rule="(has(self.sourceRef)?1:0) + (has(self.fetch)?1:0) + (has(self.image)?1:0) == 1",message="set exactly one of sourceRef, fetch or image"
// +kubebuilder:validation:XValidation:rule="!has(self.fetch) || self.fetch.unpack == 'tar' || self.fetch.unpack == 'tar.gz'",message="a build context is a directory tree, so fetch.unpack must be tar or tar.gz. unpack defaults to 'none', which places a single file, so this has to be set explicitly"
type BuildContext struct {
	// SourceRef takes the context from a Flux source's artifact.
	//
	// The one to reach for when the content moves: source-controller tracks the revision. ADR 0042
	// says which sources are delegated to it and why.
	// +optional
	SourceRef *SourceRefSource `json:"sourceRef,omitempty"`

	// Fetch retrieves the context as an archive over HTTP(S), at a declared digest.
	//
	// For a release tarball rather than a checkout. The digest is declared, not resolved: a
	// mismatch means the URL served something other than what this spec names, and is refused.
	//
	// Only the archive unpack modes apply, since a context is a tree.
	// +optional
	Fetch *FetchSource `json:"fetch,omitempty"`

	// Image takes the flattened filesystem of a digest-pinned image as the context.
	//
	// For building on what CI already published. Costs a pull and a flatten in the build pod on
	// every cache miss, so prefer sourceRef where it would do.
	//
	// `FROM <image>@sha256:… AS ctx` plus `COPY --from=ctx` does much the same with no context at
	// all. Reach for this when the image IS the tree the build reads -- notably an
	// ImageComposition's output, which is how "compose the workdir, then build it" is spelled.
	// +optional
	Image *ImageSource `json:"image,omitempty"`
}

// GetImage returns the image source this context names, or nil when it names none.
func (c *BuildContext) GetImage() *ImageSource {
	if c == nil {
		return nil
	}
	return c.Image
}

// GetSourceRef returns the Flux source this context names, or nil when it names none.
//
// Nil-safe on the receiver, because no context at all is legal.
func (c *BuildContext) GetSourceRef() *SourceRefSource {
	if c == nil {
		return nil
	}
	return c.SourceRef
}

// DockerfileSource says where the Dockerfile comes from.
//
// `path` is the common case: the recipe lives in the thing being built. `inline` puts it in this
// spec. `configMapRef` puts it in an object a platform team can own separately and share between
// several ImageBuilds.
//
// No field here carries a schema default, deliberately: a structural default is materialised into
// the stored object, so a defaulted `path` would make has(self.path) true for every object and the
// exactly-one rule below could never fire. EffectiveDockerfile holds it instead, the same
// arrangement as Push.OnConflict.
//
// +kubebuilder:validation:XValidation:rule="(has(self.path)?1:0) + (has(self.inline)?1:0) + (has(self.configMapRef)?1:0) == 1",message="set exactly one of path, inline or configMapRef"
type DockerfileSource struct {
	// Path to the Dockerfile inside the build context, resolved against the context subpath.
	// Refused without a context by the rule on ImageBuildSpec.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Path string `json:"path,omitempty"`

	// Inline is the Dockerfile itself, verbatim. Plaintext in etcd and in `kubectl get -o yaml`.
	//
	// An unpinned FROM here is terminal rather than retried: the fix is an edit to this field, and
	// the generation change it raises is what wakes the object. A `path` Dockerfile gets no such
	// event, so the same check is not terminal there.
	//
	// Capped well below what etcd would take: every watcher of every ImageBuild pays for the size
	// on every update.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	Inline string `json:"inline,omitempty"`

	// ConfigMapRef reads the Dockerfile from one key of a ConfigMap in this object's namespace.
	//
	// The content is hashed, not the resourceVersion, so an edit rebuilds and a no-op write does
	// not. The ConfigMap is watched, so that happens promptly rather than at the next interval.
	//
	// No `optional`, unlike a composition's configMap layer: a missing Dockerfile cannot produce an
	// empty build, only an object that can never become Ready.
	// +optional
	ConfigMapRef *ConfigMapKeyReference `json:"configMapRef,omitempty"`
}

// EffectiveDockerfile returns the path to use when the spec names none.
//
// Not a schema default -- see the DockerfileSource comment. The consequence is that
// spec.dockerfile.path is empty for most objects, so nothing may read it directly.
func (s *DockerfileSource) EffectiveDockerfile() string {
	if s == nil || s.Path == "" {
		return "Dockerfile"
	}
	return s.Path
}

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
//
// +kubebuilder:validation:XValidation:rule="has(self.context) || (has(self.dockerfile) && (has(self.dockerfile.inline) || has(self.dockerfile.configMapRef)))",message="with no context there is no tree to find a Dockerfile in: set spec.context, or give the Dockerfile directly with spec.dockerfile.inline or spec.dockerfile.configMapRef"
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

	// Context is the tree the Dockerfile's COPY and ADD read from.
	//
	// Optional: a Dockerfile that only declares a pinned FROM and runs commands reads no files.
	// Omitted, the build sees an empty context and any COPY fails inside BuildKit.
	// +optional
	Context *BuildContext `json:"context,omitempty"`

	// Dockerfile says where the recipe comes from. Omitted, it is "Dockerfile" at the context root
	// — see EffectiveDockerfile, and see DockerfileSource for why that default is not in the schema.
	// +optional
	Dockerfile *DockerfileSource `json:"dockerfile,omitempty"`

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
	// Optional. A build always publishes to a registry -- the Job runs in another pod and cannot
	// reach the controller's loopback-only serving endpoint (ADR 0025) -- but WHICH registry can
	// come from the operator's default rather than from here. Omit it entirely and the build
	// publishes to <default registry>/<namespace>/<name>.
	// +optional
	Push *Push `json:"push,omitempty"`
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

	// PodName of the build pod, for the FULL log.
	//
	// Not the record of why a build failed -- Message is, since ADR 0046. This pod is deleted when
	// the next retry falls due, so a pointer to it outlives neither the pod nor the failure.
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

	// Attestations records what supply-chain material is attached to Artifact, so a converged
	// reconcile can tell there is nothing to do without asking the registry.
	// +optional
	Attestations *AttestationStatus `json:"attestations,omitempty"`

	// History is the retained builds, newest first.
	// +optional
	History []BuildRecord `json:"history,omitempty"`

	// Conflict records content this object produced and did not publish, because onConflict: Keep
	// left an existing tag in place. Cleared as soon as a reconcile publishes cleanly.
	// +optional
	Conflict *TagConflictStatus `json:"conflict,omitempty"`

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
// The keep annotation is emitted into the CRD itself so the chart can install it verbatim.
// Deleting a CRD deletes every object of that kind, and Helm removing one on an uninstall or a
// toggle flip is not a risk worth taking for a resource that costs nothing when unused.
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ibuild
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=`.status.artifact.ref`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
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

// GetFetch returns the fetch source this context names, or nil when it names none.
func (c *BuildContext) GetFetch() *FetchSource {
	if c == nil {
		return nil
	}
	return c.Fetch
}
