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
// One Job per build, rootless, in the object's own namespace. A Job is an API object, so it
// survives leader failover and is adopted rather than restarted. The rejected alternatives — a
// shared BuildKit Deployment, in-process building — are in ADR 0025.

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
	// Pinning is enforced at startup, not here; the digest is part of the input hash.
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
	name := fmt.Sprintf("%s-%s", obj.Name, shortHash(inputHash))
	if len(name) > 63 {
		name = name[len(name)-63:]
	}
	return name
}

// shortHash is the human-sized form of an input hash, used for the Job name and its label so the
// two cannot drift.
func shortHash(inputHash string) string {
	short := strings.TrimPrefix(inputHash, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	return short
}

// rootlessSecurityContext is the posture every container in a build pod runs under.
//
// Privileged is not offered at any setting: ADR 0001 named that blast radius as the reason for
// refusing to build at all, and a flag reinstating it would make every other guarantee here
// conditional.
func rootlessSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                ptr.To[int64](1000),
		RunAsGroup:               ptr.To[int64](1000),
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		Privileged:               ptr.To(false),
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
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

	// rewrite-timestamp needs SOURCE_DATE_EPOCH to mean anything. Together they narrow the "same
	// inputs, different bytes" gap; ADR 0025 says why they do not close it.
	args = append(args, "--output",
		"type=image,name="+pushNames(obj)+",push=true,rewrite-timestamp=true")
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

	// Projected into the build pod rather than read by the controller, which keeps registry tokens
	// out of the controller's memory entirely for the push path.
	hasPushSecret := spec.Push != nil && spec.Push.SecretRef != nil
	if hasPushSecret {
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
	if hasPushSecret {
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: dockerPath})
	}

	// buildctl writes the pushed digest to a file in an emptyDir, which the controller cannot read.
	// Copying it to the termination log is what gets it back out: Kubernetes surfaces that in the
	// pod's container status, which is the supported channel for a small result and needs no exec
	// and no log scraping.
	//
	// The `sh -c "$@"` form passes the buildctl arguments positionally, so nothing here has to
	// quote them and an argument containing a space cannot break the script.
	script := fmt.Sprintf(`set -e
buildctl-daemonless.sh "$@"
cat %s > /dev/termination-log
`, path.Join(resultPath, metadataFile))

	container := corev1.Container{
		Name:  "build",
		Image: cfg.BuilderImage,
		// Command is the wrapper, Args the buildctl arguments it passes through as "$@".
		Command:                  []string{"sh", "-c", script, "sh"},
		Args:                     args,
		Env:                      env,
		VolumeMounts:             mounts,
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		SecurityContext:          rootlessSecurityContext(),
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
		VolumeMounts:    []corev1.VolumeMount{{Name: contextVolume, MountPath: contextPath}},
		SecurityContext: rootlessSecurityContext(),
	}
	if spec.Resources != nil {
		// The same limits as the build container. Without this the fetch is the one unbounded
		// container in the pod, which is the wrong thing to leave unbounded when it is the part
		// downloading somebody else's tarball.
		initContainer.Resources = *spec.Resources
	}

	// Enforced by Kubernetes rather than by the controller noticing: ActiveDeadlineSeconds kills
	// the pod and marks the Job Failed with DeadlineExceeded, which observeJob already surfaces.
	// A controller-side timer would have to survive a leader change to mean anything.
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
			// One attempt. BuildKit retries nothing usefully on its own, and a Job retrying a
			// failing RUN four times just delays the failure the user needs to see.
			BackoffLimit:          ptr.To[int32](0),
			ActiveDeadlineSeconds: deadline,
			// Failed Jobs linger a little so `kubectl logs` still works; the controller records
			// the pod name in status for exactly that.
			TTLSecondsAfterFinished: ptr.To[int32](3600),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: spec.ServiceAccountName,
					// No API token in the build pod unless the spec asked for an identity.
					//
					// This, not a dedicated ServiceAccount, is what "a pod running code from a git
					// repository carries no credentials" actually requires. A ServiceAccount is
					// namespaced and builds run in the object's namespace, so a chart-created one
					// could never have been reachable; suppressing the mount works everywhere and
					// needs no coordination. Naming an account is opting back in, for the case
					// where a build genuinely needs an identity.
					AutomountServiceAccountToken: automount(spec.ServiceAccountName),
					InitContainers:               []corev1.Container{initContainer},
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
// Always per-object; nothing shares one. See the doc on BuildCache.Ref for why.
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
