package buildcontroller

import (
	"fmt"
	"path"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// Turning an ImageBuild into a Job: one rootless Job per build, in the object's own namespace, so
// it survives leader failover and is adopted rather than restarted. ADR 0025.

const (
	// contextVolume holds the fetched build context; resultVolume carries the metadata file back.
	contextVolume = "context"
	resultVolume  = "result"
	secretVolume  = "build-secrets"
	dockerVolume  = "docker-config"

	contextPath = "/workspace"
	resultPath  = "/result"

	// A Dockerfile from outside the context is projected here under a fixed name.
	dockerfileVolume = "dockerfile"
	dockerfilePath   = "/dockerfile"
	dockerfileName   = "Dockerfile"
	dockerfileKey    = "Dockerfile"
	secretPath       = "/secrets"

	// The per-build context token, projected via subPath as a plain file.
	contextTokenVolume = "context-token"
	contextTokenPath   = "/context-token"
	contextTokenFile   = "token"
	// The copied registry CA, and the merged bundle. The bundle is an emptyDir because uid 1000
	// cannot write to the image's /etc/ssl/certs.
	registryCAPath = "/registry-ca"
	caBundlePath   = "/certs/ca-bundle.crt"
	dockerPath     = "/docker"

	// metadataFile is where buildctl writes the pushed digest.
	metadataFile = "metadata.json"

	// InputHashLabel lets the controller find the Job for a set of inputs without reading status,
	// so it can adopt it after a restart.
	InputHashLabel = "oci.lhns.de/input-hash"
	// ManagedByLabel marks Jobs this controller owns.
	ManagedByLabel = "app.kubernetes.io/managed-by"
)

// JobConfig is the operator-level configuration a build needs.
type JobConfig struct {
	// BuilderImage is the rootless BuildKit image, pinned by digest (enforced at startup). Part of
	// the input hash.
	BuilderImage string
	// FrontendImage is the Dockerfile frontend, pinned by digest, so `# syntax=` is not resolved
	// over the network.
	FrontendImage string
	// FetcherImage runs `oci-builder fetch-context` as the init container. In the input hash: a
	// fixed unpack bug can change the tree under an unchanged context digest.
	FetcherImage string
	// SBOM and Provenance turn on BuildKit's attestations. In the input hash, because they change
	// what is pushed.
	SBOM       bool
	Provenance bool

	// RegistryCA is a PEM bundle the build must trust, merged with the image's own roots. Empty
	// when the registry is already trusted. Not in the input hash: transport, not content.
	RegistryCA []byte

	// InsecureRegistries are registry hosts to talk to over plain HTTP. Not in the input hash:
	// transport, not content.
	InsecureRegistries []string
	// ContextBaseURL is where a build pod fetches its Flux context: this controller's proxy. Empty
	// means fetch from source-controller directly (as in tests). Not in the input hash.
	ContextBaseURL string

	// SourceDateEpoch is the timestamp stamped into the result. Zero by default, matching the
	// composer's fixed epoch.
	SourceDateEpoch string
}

// jobName is deterministic in the object and its inputs, so a second leader or a restarted
// controller gets AlreadyExists or adopts the existing Job instead of starting another build.
func jobName(obj *ociv1alpha1.ImageBuild, inputHash string) string {
	name := fmt.Sprintf("%s-%s", obj.Name, shortHash(inputHash))
	if len(name) > 63 {
		name = name[len(name)-63:]
	}
	return name
}

// shortHash is the human-sized form of an input hash, shared by the Job name and label.
func shortHash(inputHash string) string {
	short := strings.TrimPrefix(inputHash, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	return short
}

// rootlessSecurityContext is the posture every container in a build pod runs under. Never
// privileged (ADR 0001).
//
// Rootless BuildKit maps a UID range, which needs the setuid `newuidmap` and CAP_SETUID/SETGID;
// either allowPrivilegeEscalation: false or dropping those capabilities stops buildkitd from
// starting. ADR 0027.
func rootlessSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:    ptr.To[int64](1000),
		RunAsGroup:   ptr.To[int64](1000),
		RunAsNonRoot: ptr.To(true),
		Privileged:   ptr.To(false),
		// Required for setuid newuidmap; limited by the capabilities below.
		AllowPrivilegeEscalation: ptr.To(true),
		// Required by rootless BuildKit to create user namespaces and mount inside them. Build pod
		// only; the controller stays locked down.
		SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		// Everything dropped, then only what UID/GID mapping needs.
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"SETUID", "SETGID"},
		},
	}
}

// fetchContextArgs is what the init container is told to fetch. See internal/fetchcontext.
func fetchContextArgs(obj *ociv1alpha1.ImageBuild, cfg JobConfig, inputHash, contextURL,
	contextDigest string) []string {

	args := []string{
		"fetch-context",
		"--dest=" + contextPath,
		"--url=" + contextURL,
		"--digest=" + contextDigest,
	}
	// Re-checks the FROM lines in the extracted tree, when the Dockerfile lives there.
	if !projectedDockerfile(obj) {
		args = append(args, "--dockerfile="+obj.Spec.Dockerfile.EffectiveDockerfile())
	}
	if ref := obj.Spec.Context.GetSourceRef(); ref != nil {
		// Through this controller's context proxy, not source-controller, which serves every
		// namespace's artifacts unauthenticated. ADR 0044. Without a configured endpoint the pod
		// falls back to source-controller directly.
		if cfg.ContextBaseURL != "" {
			args[2] = fmt.Sprintf("--url=%s/contexts/%s/%s/%s",
				strings.TrimSuffix(cfg.ContextBaseURL, "/"), obj.Namespace, obj.Name, inputHash)
			args = append(args, "--token-file="+path.Join(contextTokenPath, contextTokenFile))
		}
		return append(args, "--kind=sourceRef", "--unpack=tar.gz", "--subpath="+ref.Subpath)
	}
	if img := obj.Spec.Context.GetImage(); img != nil {
		// An image is flattened, not unpacked; --url is the pinned reference.
		return append(args, "--kind=image", "--subpath="+img.Subpath)
	}
	f := obj.Spec.Context.GetFetch()
	return append(args, "--kind=fetch", "--unpack="+string(f.Unpack), "--subpath="+f.Subpath,
		fmt.Sprintf("--strip-components=%d", f.StripComponents))
}

// buildctlArgs assembles the buildctl invocation.
func buildctlArgs(obj *ociv1alpha1.ImageBuild, cfg JobConfig, repo string, cacheAvailable bool) []string {
	spec := obj.Spec
	// Separate `context` and `dockerfile` locals. A projected Dockerfile is not copied into the
	// context, which could overwrite one already there.
	dockerfileLocal, filename := path.Join(contextPath, path.Dir(spec.Dockerfile.EffectiveDockerfile())),
		path.Base(spec.Dockerfile.EffectiveDockerfile())
	if projectedDockerfile(obj) {
		dockerfileLocal, filename = dockerfilePath, dockerfileName
	}

	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + contextPath,
		"--local", "dockerfile=" + dockerfileLocal,
		"--opt", "filename=" + filename,
		"--opt", "build-arg:BUILDKIT_SYNTAX=" + cfg.FrontendImage,
		"--metadata-file", path.Join(resultPath, metadataFile),
	}

	if spec.Target != "" {
		args = append(args, "--opt", "target="+spec.Target)
	}
	args = append(args, "--opt", "platform="+strings.Join(spec.Platforms, ","))

	for _, a := range spec.Args {
		args = append(args, "--opt", "build-arg:"+a.Name+"="+a.Value)
	}
	for _, s := range spec.Secrets {
		key := s.Key
		if key == "" {
			key = s.ID
		}
		args = append(args, "--secret",
			fmt.Sprintf("id=%s,src=%s", s.ID, path.Join(secretPath, s.SecretRef.Name, key)))
	}

	if spec.Network == "None" {
		args = append(args, "--opt", "no-network=true")
	}

	// rewrite-timestamp with SOURCE_DATE_EPOCH narrows, but does not close, the reproducibility
	// gap (ADR 0025). oci-mediatypes: BuildKit defaults to Docker media types, which OCI-native
	// registries such as zot reject with 415. push-by-digest: the Job pushes content only; the
	// controller tags afterwards, which is where onConflict is enforced (ADR 0054).
	args = append(args, "--output",
		"type=image,name="+repo+",push=true,push-by-digest=true,rewrite-timestamp=true,oci-mediatypes=true"+
			insecureAttr(repo, cfg.InsecureRegistries))
	args = append(args, "--opt", "build-arg:SOURCE_DATE_EPOCH="+cfg.SourceDateEpoch)

	// BuildKit's own attestations (ADR 0008). They turn the output into an image index, so
	// status.artifact.digest names an index, and provenance carries wall-clock timestamps, so the
	// index digest differs on every run of identical inputs.
	if cfg.SBOM {
		args = append(args, "--opt", "attest:sbom=")
	}
	if cfg.Provenance {
		args = append(args, "--opt", "attest:provenance=mode=min")
	}

	if cacheRef := cacheRefFor(obj, repo); cacheRef != "" {
		insecure := insecureAttr(repo, cfg.InsecureRegistries)
		// Import only when the cache exists: BuildKit treats an unresolvable cache ref as fatal,
		// which would fail every first build on some registries (e.g. zot).
		if cacheAvailable {
			args = append(args, "--import-cache", "type=registry,ref="+cacheRef+insecure)
		}
		// Export always, to create the cache. image-manifest=true with oci-mediatypes=true stores
		// it as a plain OCI image manifest any registry accepts.
		args = append(args, "--export-cache",
			"type=registry,ref="+cacheRef+",mode=max,oci-mediatypes=true,image-manifest=true"+insecure)
	}

	return args
}

// insecureAttr returns the exporter attribute that allows plain HTTP, only when the push target's
// host is one the operator listed.
func insecureAttr(repository string, insecure []string) string {
	if repository == "" || !recon.InsecureHost(repository, insecure) {
		return ""
	}
	return ",registry.insecure=true"
}

// projectedDockerfile reports whether the Dockerfile is carried into the pod rather than found in
// the context. The single predicate for both the volume and the buildctl --local, so they agree.
func projectedDockerfile(obj *ociv1alpha1.ImageBuild) bool {
	df := obj.Spec.Dockerfile
	return df != nil && (df.Inline != "" || df.ConfigMapRef != nil)
}

// buildVolumes returns the pod's volumes and the build container's mounts, built in pairs so they
// agree.
func buildVolumes(obj *ociv1alpha1.ImageBuild, pushSecret, dockerfileSecret string) ([]corev1.Volume, []corev1.VolumeMount) {
	spec := obj.Spec
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	add := func(name string, src corev1.VolumeSource, at string, readOnly bool) {
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: src})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: at, ReadOnly: readOnly})
	}
	empty := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}

	add(contextVolume, empty, contextPath, false)
	add(resultVolume, empty, resultPath, false)

	// The checked Dockerfile, via subPath: a Secret volume is otherwise a `..data` symlink farm,
	// which fsutil walks rather than flattens. subPath also means no re-projection mid-build.
	if projectedDockerfile(obj) {
		volumes = append(volumes, corev1.Volume{
			Name: dockerfileVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: dockerfileSecret,
					Items:      []corev1.KeyToPath{{Key: dockerfileKey, Path: dockerfileName}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      dockerfileVolume,
			MountPath: path.Join(dockerfilePath, dockerfileName),
			SubPath:   dockerfileName,
			ReadOnly:  true,
		})
	}

	// Push credentials go to the pod, never into the controller's memory. The Secret is the
	// object's own or a per-build copy of the operator's, in the build's namespace.
	if pushSecret != "" {
		add(dockerVolume, corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: pushSecret,
				Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
			},
		}, dockerPath, true)
	}

	for _, sec := range spec.Secrets {
		add(secretVolume+"-"+sec.SecretRef.Name, corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: sec.SecretRef.Name},
		}, path.Join(secretPath, sec.SecretRef.Name), true)
	}

	return volumes, mounts
}

// registryCAVolumes mounts the copied CA and a writable place to merge it with the image's roots.
// Gated on the same condition as the env var and script prelude in buildJob.
func registryCAVolumes(caSecret string) ([]corev1.Volume, []corev1.VolumeMount) {
	if caSecret == "" {
		return nil, nil
	}
	return []corev1.Volume{
			{
				Name:         "registry-ca",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: caSecret}},
			},
			{
				Name:         "ca-bundle",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		}, []corev1.VolumeMount{
			{Name: "registry-ca", MountPath: registryCAPath, ReadOnly: true},
			{Name: "ca-bundle", MountPath: path.Dir(caBundlePath)},
		}
}

// buildJob renders the Job for one build.
func buildJob(obj *ociv1alpha1.ImageBuild, inputHash, contextURL, contextDigest string, cfg JobConfig,
	repo, pushSecret, caSecret, dockerfileSecret, contextSecret string, cacheAvailable bool) *batchv1.Job {

	spec := obj.Spec
	args := buildctlArgs(obj, cfg, repo, cacheAvailable)
	volumes, mounts := buildVolumes(obj, pushSecret, dockerfileSecret)
	caVolumes, caMounts := registryCAVolumes(caSecret)
	volumes = append(volumes, caVolumes...)
	mounts = append(mounts, caMounts...)
	hasPushSecret := pushSecret != ""

	env := []corev1.EnvVar{
		{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
		{Name: "SOURCE_DATE_EPOCH", Value: cfg.SourceDateEpoch},
	}
	if hasPushSecret {
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: dockerPath})
	}
	if caSecret != "" {
		// Covers both buildctl and buildkitd (both Go).
		env = append(env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: caBundlePath})
	}

	// SSL_CERT_FILE replaces Go's system pool, so the registry CA is merged with the system bundle
	// or public registries would stop verifying. `|| true` tolerates an image without a system
	// bundle under `set -e`. /etc/buildkit/certs/<host>/ca.pem is ignored by rootless BuildKit
	// (moby/buildkit#6406).
	caPrelude := ""
	if caSecret != "" {
		caPrelude = fmt.Sprintf(`{ cat /etc/ssl/certs/ca-certificates.crt 2>/dev/null || true; cat %s/ca.crt; } > %s
`, registryCAPath, caBundlePath)
	}
	// The metadata file is copied to the termination log, which the controller reads from the pod
	// status. `sh -c "$@"` passes the buildctl arguments positionally, so nothing needs quoting.
	script := fmt.Sprintf(`set -e
%sbuildctl-daemonless.sh "$@"
cat %s > /dev/termination-log
`, caPrelude, path.Join(resultPath, metadataFile))

	container := corev1.Container{
		Name:  "build",
		Image: cfg.BuilderImage,
		// Command is the wrapper, Args the buildctl arguments it passes through as "$@".
		Command:                []string{"sh", "-c", script, "sh"},
		Args:                   args,
		Env:                    env,
		VolumeMounts:           mounts,
		TerminationMessagePath: corev1.TerminationMessagePathDefault,
		// On success the message is the digest (podBuildDigest); on failure the kubelet falls back
		// to the log tail, giving a cause without a pods/log grant. ADR 0046.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext:          rootlessSecurityContext(),
	}
	if spec.Resources != nil {
		container.Resources = *spec.Resources
	}

	// The context is fetched by an init container, never by the controller. No context, no init
	// container (fetchContextArgs would dereference nil).
	var initContainers []corev1.Container
	if obj.Spec.Context != nil {
		// Our own binary: it fetches, verifies the digest, then extracts. FallbackToLogsOnError so
		// a fetch failure's cause reaches status.
		fetch := corev1.Container{
			Name:                     "fetch-context",
			Image:                    cfg.FetcherImage,
			Args:                     fetchContextArgs(obj, cfg, inputHash, contextURL, contextDigest),
			VolumeMounts:             fetchMounts(contextSecret),
			SecurityContext:          rootlessSecurityContext(),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}
		if spec.Resources != nil {
			// Bound the container that downloads untrusted content too.
			fetch.Resources = *spec.Resources
		}
		initContainers = append(initContainers, fetch)
	}

	if contextSecret != "" {
		vol, _ := contextTokenProjection(contextSecret)
		volumes = append(volumes, vol)
	}

	// Enforced by Kubernetes, which survives a leader change; observeJob surfaces DeadlineExceeded.
	var deadline *int64
	if spec.Timeout != nil && spec.Timeout.Duration > 0 {
		deadline = ptr.To(int64(spec.Timeout.Seconds()))
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(obj, inputHash),
			Namespace: obj.Namespace,
			Labels: map[string]string{
				ManagedByLabel: "kube-oci-builder",
				InputHashLabel: shortHash(inputHash),
			},
		},
		Spec: batchv1.JobSpec{
			// One attempt: retrying a failing RUN only delays the failure. The controller retries.
			BackoffLimit:          ptr.To[int32](0),
			ActiveDeadlineSeconds: deadline,
			// Linger so `kubectl logs` still works on a failed build.
			TTLSecondsAfterFinished: ptr.To[int32](3600),
			Template: corev1.PodTemplateSpec{
				// Pod labels too, so NetworkPolicies, quotas and admission rules in tenant
				// namespaces can select build pods as a class.
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						ManagedByLabel: "kube-oci-builder",
						InputHashLabel: shortHash(inputHash),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: spec.ServiceAccountName,
					// No API token unless the spec names an identity: the pod runs untrusted code.
					AutomountServiceAccountToken: automount(spec.ServiceAccountName),
					InitContainers:               initContainers,
					Containers:                   []corev1.Container{container},
					Volumes:                      volumes,
				},
			},
		},
	}
}

// automount reports whether the build pod should receive an API token: only when the spec named an
// account on purpose.
func automount(serviceAccount string) *bool {
	if serviceAccount == "" {
		return ptr.To(false)
	}
	return nil
}

// cacheRefFor returns where this object's build cache lives, or "" when caching is disabled.
// Always per-object; see BuildCache.Ref.
func cacheRefFor(obj *ociv1alpha1.ImageBuild, repo string) string {
	cache := obj.Spec.Cache
	if cache != nil && cache.Mode == "Disabled" {
		return ""
	}
	if cache != nil && cache.Ref != "" {
		return cache.Ref
	}
	if repo == "" {
		return ""
	}
	return fmt.Sprintf("%s-buildcache-%s-%s", repo, obj.Namespace, obj.Name)
}

// contextTokenProjection is the token volume and the mount that names it, via subPath as a plain
// file.
func contextTokenProjection(secret string) (corev1.Volume, corev1.VolumeMount) {
	return corev1.Volume{
			Name: contextTokenVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secret,
					Items:      []corev1.KeyToPath{{Key: contextTokenKey, Path: contextTokenFile}},
				},
			},
		}, corev1.VolumeMount{
			Name:      contextTokenVolume,
			MountPath: path.Join(contextTokenPath, contextTokenFile),
			SubPath:   contextTokenFile,
			ReadOnly:  true,
		}
}

// fetchMounts is what the context fetcher mounts. The token is mounted only here, never in the
// build container, where every RUN line could read it.
func fetchMounts(contextSecret string) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{{Name: contextVolume, MountPath: contextPath}}
	if contextSecret == "" {
		return mounts
	}
	_, mount := contextTokenProjection(contextSecret)
	return append(mounts, mount)
}
