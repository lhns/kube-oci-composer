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
)

// Turning a DockerBuild into a Job.
//
// One Job per build, rootless, in the object's own namespace. ADR 0025 records why this rather
// than a shared BuildKit Deployment or an in-process builder: a shared daemon makes the controller
// a confused deputy holding credentials for every namespace and turns its cache into a channel
// between tenants, and in-process building destroys readOnlyRootFilesystem, drop: [ALL], non-root
// and distroless in one move — which is the posture ADR 0001 named when it refused to build at
// all. A Job is also an API object, so it survives leader failover and is adopted rather than
// restarted.

const (
	// contextVolume holds the fetched build context; resultVolume carries the metadata file back.
	contextVolume = "context"
	resultVolume  = "result"
	secretVolume  = "build-secrets"
	dockerVolume  = "docker-config"

	contextPath = "/workspace"
	resultPath  = "/result"
	secretPath  = "/secrets"
	dockerPath  = "/docker"

	// metadataFile is where buildctl writes the pushed digest, which is the one thing the
	// controller needs back out of the build.
	metadataFile = "metadata.json"

	// InputHashLabel lets the controller find the Job for a given set of inputs without reading
	// status, which is what makes adoption after a restart work.
	InputHashLabel = "oci.lhns.de/input-hash"
	// ManagedByLabel marks Jobs this controller owns.
	ManagedByLabel = "app.kubernetes.io/managed-by"
)

// JobConfig is the operator-level configuration a build needs.
type JobConfig struct {
	// BuilderImage is the rootless BuildKit image, pinned by digest.
	//
	// Pinned is enforced at startup rather than here: its digest is in the input hash, playing the
	// role oci.AssemblyVersion plays for the composer, so an unpinned value would make the hash a
	// lie. See ADR 0025.
	BuilderImage string
	// FrontendImage is the Dockerfile frontend, pinned by digest. BuildKit resolves `# syntax=`
	// over the network unless told otherwise.
	FrontendImage string
	// SourceDateEpoch is the timestamp stamped into the result. Zero by default, matching the
	// composer's fixed epoch.
	SourceDateEpoch string
}

// jobName is deterministic in the object and its inputs.
//
// That is what makes a brief two-leader window harmless: the second Create gets AlreadyExists
// rather than starting a second build, and a controller that restarts mid-build finds the Job it
// left behind instead of duplicating it.
func jobName(obj *ociv1alpha1.DockerBuild, inputHash string) string {
	short := strings.TrimPrefix(inputHash, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	name := fmt.Sprintf("%s-%s", obj.Name, short)
	if len(name) > 63 {
		name = name[len(name)-63:]
	}
	return name
}

// buildJob renders the Job for one build.
func buildJob(obj *ociv1alpha1.DockerBuild, inputHash, contextURL string, cfg JobConfig) *batchv1.Job {
	spec := obj.Spec

	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + contextPath,
		"--local", "dockerfile=" + path.Join(contextPath, path.Dir(spec.Dockerfile)),
		"--opt", "filename=" + path.Base(spec.Dockerfile),
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

	// The exporter. rewrite-timestamp needs SOURCE_DATE_EPOCH to mean anything, and both are what
	// narrow — without closing — the "same inputs, different bytes" gap ADR 0025 documents.
	output := []string{
		"type=image",
		"name=" + pushNames(obj),
		"push=true",
		"rewrite-timestamp=true",
	}
	args = append(args, "--output", strings.Join(output, ","))
	args = append(args, "--opt", "build-arg:SOURCE_DATE_EPOCH="+cfg.SourceDateEpoch)

	if cacheRef := cacheRefFor(obj); cacheRef != "" {
		args = append(args,
			"--import-cache", "type=registry,ref="+cacheRef,
			"--export-cache", "type=registry,ref="+cacheRef+",mode=max")
	}

	volumes := []corev1.Volume{
		{Name: contextVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: resultVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: contextVolume, MountPath: contextPath},
		{Name: resultVolume, MountPath: resultPath},
	}

	// Push credentials are projected into the build pod rather than read by the controller. That
	// keeps registry tokens out of the controller's memory entirely for the push path.
	if spec.Push != nil && spec.Push.SecretRef != nil {
		volumes = append(volumes, corev1.Volume{
			Name: dockerVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: spec.Push.SecretRef.Name,
					Items: []corev1.KeyToPath{
						{Key: corev1.DockerConfigJsonKey, Path: "config.json"},
					},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: dockerVolume, MountPath: dockerPath, ReadOnly: true})
	}

	for _, s := range spec.Secrets {
		volumes = append(volumes, corev1.Volume{
			Name: secretVolume + "-" + s.SecretRef.Name,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: s.SecretRef.Name},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      secretVolume + "-" + s.SecretRef.Name,
			MountPath: path.Join(secretPath, s.SecretRef.Name),
			ReadOnly:  true,
		})
	}

	env := []corev1.EnvVar{
		{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
		{Name: "SOURCE_DATE_EPOCH", Value: cfg.SourceDateEpoch},
	}
	if spec.Push != nil && spec.Push.SecretRef != nil {
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: dockerPath})
	}

	container := corev1.Container{
		Name:         "build",
		Image:        cfg.BuilderImage,
		Command:      []string{"buildctl-daemonless.sh"},
		Args:         args,
		Env:          env,
		VolumeMounts: mounts,
		SecurityContext: &corev1.SecurityContext{
			// Rootless. Privileged is not offered at any setting: ADR 0001:56-57 named that blast
			// radius as the reason for refusing to build in the first place, and a flag that
			// reinstates it would make every other guarantee here conditional.
			RunAsUser:                ptr.To[int64](1000),
			RunAsGroup:               ptr.To[int64](1000),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			Privileged:               ptr.To(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if spec.Resources != nil {
		container.Resources = *spec.Resources
	}

	// The context is fetched by an init container rather than by the controller: the controller
	// would otherwise have to hold the whole context in memory or on its own read-only filesystem,
	// and the URL is already a digest-addressed artifact that anything can pull.
	initContainer := corev1.Container{
		Name:  "fetch-context",
		Image: cfg.BuilderImage,
		Command: []string{"sh", "-c",
			fmt.Sprintf("wget -qO- %q | tar -xzf - -C %s", contextURL, contextPath)},
		VolumeMounts: []corev1.VolumeMount{{Name: contextVolume, MountPath: contextPath}},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To[int64](1000),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(obj, inputHash),
			Namespace: obj.Namespace,
			Labels: map[string]string{
				ManagedByLabel: "kube-oci-builder",
				InputHashLabel: strings.TrimPrefix(inputHash, "sha256:")[:12],
			},
		},
		Spec: batchv1.JobSpec{
			// One attempt. BuildKit retries nothing usefully on its own, and a Job retrying a
			// failing RUN four times just delays the failure the user needs to see.
			BackoffLimit: ptr.To[int32](0),
			// Failed Jobs linger a little so `kubectl logs` still works; the controller records
			// the pod name in status for exactly that.
			TTLSecondsAfterFinished: ptr.To[int32](3600),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: spec.ServiceAccountName,
					InitContainers:     []corev1.Container{initContainer},
					Containers:         []corev1.Container{container},
					Volumes:            volumes,
				},
			},
		},
	}
}

// pushNames renders the comma-separated image names the exporter pushes to.
func pushNames(obj *ociv1alpha1.DockerBuild) string {
	if obj.Spec.Push == nil {
		return ""
	}
	repo := obj.Spec.Push.Repository
	if len(obj.Spec.Push.Tags) == 0 {
		return repo
	}
	names := make([]string, 0, len(obj.Spec.Push.Tags))
	for _, tag := range obj.Spec.Push.Tags {
		names = append(names, repo+":"+tag)
	}
	return strings.Join(names, ",")
}

// cacheRefFor returns where this object's build cache lives, or "" when caching is disabled.
//
// Always per-object. A cache shared between objects is a channel between whoever can write their
// Dockerfiles, so there is no setting that shares one.
func cacheRefFor(obj *ociv1alpha1.DockerBuild) string {
	cache := obj.Spec.Cache
	if cache != nil && cache.Mode == "Disabled" {
		return ""
	}
	if cache != nil && cache.Ref != "" {
		return cache.Ref
	}
	if obj.Spec.Push == nil {
		return ""
	}
	return fmt.Sprintf("%s-buildcache-%s-%s", obj.Spec.Push.Repository, obj.Namespace, obj.Name)
}
