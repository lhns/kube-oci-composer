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

// Turning an ImageBuild into a Job.
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
	// Where the copied registry CA is mounted, and where the merged bundle is written. The bundle
	// is an emptyDir because uid 1000 cannot write to the image's root-owned /etc/ssl/certs.
	registryCAPath = "/registry-ca"
	caBundlePath   = "/certs/ca-bundle.crt"
	dockerPath     = "/docker"

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
	// RegistryCA is a PEM bundle the build must trust, copied into the build's namespace and
	// merged with the image's own roots. Empty when the registry's certificate is already trusted.
	//
	// Deliberately NOT part of the input hash. How the bytes are transported does not change what
	// they are -- the same note InsecureRegistries carries below.
	RegistryCA []byte

	// InsecureRegistries are registry hosts to talk to over plain HTTP.
	//
	// Operator-level and opt-in per host, not a global "trust anything": an internal or air-gapped
	// registry without TLS is a real deployment, and the alternative is telling those clusters to
	// use a different tool. It is deliberately NOT part of the input hash — how the bytes are
	// transported does not change what they are, so flipping it must not rebuild anything.
	InsecureRegistries []string
	// SourceDateEpoch is the timestamp stamped into the result. Zero by default, matching the
	// composer's fixed epoch.
	SourceDateEpoch string
}

// jobName is deterministic in the object and its inputs.
//
// That is what makes a brief two-leader window harmless: the second Create gets AlreadyExists
// rather than starting a second build, and a controller that restarts mid-build finds the Job it
// left behind instead of duplicating it.
func jobName(obj *ociv1alpha1.ImageBuild, inputHash string) string {
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
// conditional. Nothing below grants host access, device access or host mounts.
//
// Rootless BuildKit maps a RANGE of UIDs so a build can create files owned by root and by package
// users, and the kernel lets an unprivileged process map only ONE by itself; the range needs
// CAP_SETUID, which is why the image ships setuid-root `newuidmap`. Both `allowPrivilegeEscalation:
// false` and `drop: ALL` independently stop that working, and buildkitd then never starts. Measured
// rather than chosen — ADR 0027.
func rootlessSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:    ptr.To[int64](1000),
		RunAsGroup:   ptr.To[int64](1000),
		RunAsNonRoot: ptr.To(true),
		Privileged:   ptr.To(false),
		// Required for setuid newuidmap, and it buys only what the two capabilities below allow —
		// this is not privileged, and the container is still uid 1000.
		AllowPrivilegeEscalation: ptr.To(true),
		// Seccomp and AppArmor unconfined are what rootless BuildKit documents as required: it
		// creates user namespaces and mounts inside them, and both defaults block that. This is
		// loosened for the BUILD pod only — the controller keeps distroless, non-root and a
		// read-only root filesystem.
		SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		// Everything dropped, then exactly the two the UID/GID mapping needs. Upstream's own
		// Kubernetes example ships no capabilities stanza at all, which leaves the runtime's
		// default set — around fourteen, including CHOWN, DAC_OVERRIDE and FOWNER.
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"SETUID", "SETGID"},
		},
	}
}

// fetchContextScript downloads the build context and unwraps it to the directory buildctl reads.
//
// The unwrapping is the whole reason this is a script rather than one pipe. A source-controller
// artifact wraps the tree in a single top-level directory whose name nobody can predict, so a plain
// extraction leaves the Dockerfile one level below where buildctl looks for it.
//
// Deliberately the SAME rule as build.matchesContextPath, which strips that wrapper controller-side:
// when the two disagreed, an unpinned FROM was correctly refused and every build that passed the
// check then failed inside BuildKit. Strip one level only when the archive really is a single
// wrapper directory, so a tarball whose files sit at the root still builds rather than being
// silently emptied.
func fetchContextScript(contextURL string) string {
	return fmt.Sprintf(`set -e
staging=%[2]s/.staging
mkdir -p "$staging"
wget -qO- %[1]q | tar -xzf - -C "$staging"

src="$staging"
if [ "$(ls -A "$staging" | wc -l)" -eq 1 ]; then
  only="$staging/$(ls -A "$staging")"
  [ -d "$only" ] && src="$only"
fi

# tar rather than mv: it copies dotfiles without a shell glob that misses them.
tar -cf - -C "$src" . | tar -xf - -C %[2]s
rm -rf "$staging"
`, contextURL, contextPath)
}

// buildctlArgs assembles the buildctl invocation. Split out because it is the part that decides
// what gets built, and the only part the argv tests read.
func buildctlArgs(obj *ociv1alpha1.ImageBuild, cfg JobConfig, repo string, cacheAvailable bool) []string {
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
	// oci-mediatypes=true, because BuildKit otherwise emits DOCKER media types and an OCI-native
	// registry answers a manifest PUT with 415 Unsupported Media Type. zot does exactly that.
	//
	// Not an accommodation for one registry. This project is OCI-oriented throughout, the composer
	// already writes OCI manifests, and the two kinds emitting different media types into the same
	// registry is the kind of divergence the rest of this work has been removing. The Docker types
	// were never chosen here; they were BuildKit's default and nothing had contradicted it.
	args = append(args, "--output",
		"type=image,name="+pushNames(obj, repo)+",push=true,rewrite-timestamp=true,oci-mediatypes=true"+
			insecureAttr(repo, cfg.InsecureRegistries))
	args = append(args, "--opt", "build-arg:SOURCE_DATE_EPOCH="+cfg.SourceDateEpoch)

	if cacheRef := cacheRefFor(obj, repo); cacheRef != "" {
		insecure := insecureAttr(repo, cfg.InsecureRegistries)
		// Import ONLY when the cache reference actually resolves. BuildKit configures the registry
		// cache importer eagerly, and a reference it cannot resolve is a fatal error rather than a
		// warning -- so passing this unconditionally fails every build whose cache does not exist
		// yet, which is every FIRST build.
		//
		// That went unnoticed for as long as the e2e ran against registry:2, whose answer for a
		// missing manifest BuildKit happened to tolerate. zot's is not, and the difference is not
		// something to depend on either way: a missing build cache must never fail a build, whatever
		// the registry replies.
		if cacheAvailable {
			args = append(args, "--import-cache", "type=registry,ref="+cacheRef+insecure)
		}
		// Export unconditionally: this is what creates the cache the next build imports.
		//
		// image-manifest=true with oci-mediatypes=true for the same reason as the image above, and
		// then some: BuildKit's default cache format is a manifest LIST carrying a config a
		// spec-conformant registry has no obligation to accept. The pair renders the cache as an
		// ordinary OCI image manifest, which any registry can store.
		args = append(args, "--export-cache",
			"type=registry,ref="+cacheRef+",mode=max,oci-mediatypes=true,image-manifest=true"+insecure)
	}

	return args
}

// insecureAttr returns the exporter attribute that allows plain HTTP, when the push target's host
// is one the operator listed.
//
// Matched on host rather than applied globally, so naming one internal registry does not quietly
// downgrade every other push the same controller makes.
func insecureAttr(repository string, insecure []string) string {
	if repository == "" || !recon.InsecureHost(repository, insecure) {
		return ""
	}
	return ",registry.insecure=true"
}

// insecureHost reports whether a repository's host is on the operator's allow-list for plain HTTP.
//
// Shared with the controller's own registry reads, so a host it can push to insecurely is one it
// can also HEAD insecurely. Diverging would leave onConflict unenforceable against exactly the
// registries an e2e or air-gapped setup runs.
// buildVolumes returns the pod's volumes and the build container's mounts.
//
// Paired through one closure rather than two appends per source: a volume and the mount that names
// it have to agree, and building them in separate lists is how they stop agreeing.
func buildVolumes(spec ociv1alpha1.ImageBuildSpec, pushSecret string) ([]corev1.Volume, []corev1.VolumeMount) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	add := func(name string, src corev1.VolumeSource, at string, readOnly bool) {
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: src})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: at, ReadOnly: readOnly})
	}
	empty := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}

	add(contextVolume, empty, contextPath, false)
	add(resultVolume, empty, resultPath, false)

	// Push credentials are projected into the build pod rather than read by the controller, which
	// keeps registry tokens out of the controller's memory entirely for the push path.
	//
	// The NAME is resolved by the controller: either the object's own secretRef, or a short-lived
	// copy of the operator's credential that lives exactly as long as this Job. A pod can only mount
	// Secrets from its own namespace, and the build must run in the object's namespace -- it mounts
	// that namespace's build secrets and runs that namespace's code.
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
//
// Returned together with the env var and rendered alongside the script prelude, all three gated on
// the same condition, so a test can assert on the rendered container instead of on runtime
// behaviour. A `[ -f ... ]` check in the shell instead would silently no-op if a mount name drifted.
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
func buildJob(obj *ociv1alpha1.ImageBuild, inputHash, contextURL string, cfg JobConfig,
	repo, pushSecret, caSecret string, cacheAvailable bool) *batchv1.Job {

	spec := obj.Spec
	args := buildctlArgs(obj, cfg, repo, cacheAvailable)
	volumes, mounts := buildVolumes(spec, pushSecret)
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
		// Both buildctl and the buildkitd it forks are Go binaries, so one variable covers both.
		env = append(env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: caBundlePath})
	}

	// buildctl writes the pushed digest to a file in an emptyDir, which the controller cannot read.
	// Copying it to the termination log is what gets it back out: Kubernetes surfaces that in the
	// pod's container status, which is the supported channel for a small result and needs no exec
	// and no log scraping.
	//
	// The `sh -c "$@"` form passes the buildctl arguments positionally, so nothing here has to
	// quote them and an argument containing a space cannot break the script.
	// The CA prelude, when there is one.
	//
	// SSL_CERT_FILE REPLACES Go's system pool rather than adding to it, so pointing it straight at
	// the registry's CA would leave the build unable to verify docker.io -- and every `FROM
	// alpine` and every frontend fetch would fail. Hence the merge.
	//
	// Written at runtime because a mount cannot be merged at render time, and possible because the
	// build container does NOT set readOnlyRootFilesystem: rootlessSecurityContext deliberately
	// omits it, and the comment there says the read-only rootfs is the controller's property.
	//
	// Braces and `|| true` because the script runs under `set -e` and a builder image without a
	// system bundle must not fail the build before it starts.
	//
	// Not `/etc/buildkit/certs/<host>/ca.pem`: rootless BuildKit ignores it (moby/buildkit#6406).
	caPrelude := ""
	if caSecret != "" {
		caPrelude = fmt.Sprintf(`{ cat /etc/ssl/certs/ca-certificates.crt 2>/dev/null || true; cat %s/ca.crt; } > %s
`, registryCAPath, caBundlePath)
	}
	script := fmt.Sprintf(`set -e
%sbuildctl-daemonless.sh "$@"
cat %s > /dev/termination-log
`, caPrelude, path.Join(resultPath, metadataFile))

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
		Name:            "fetch-context",
		Image:           cfg.BuilderImage,
		Command:         []string{"sh", "-c", fetchContextScript(contextURL)},
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
				// Labels on the POD, not only on the Job.
				//
				// Kubernetes adds its own `job-name` and `controller-uid`, which is enough to find
				// a pod and not enough to describe it. Anything selecting build pods as a CLASS --
				// a NetworkPolicy letting them reach the registry, a quota, an admission rule --
				// needs a label that says what they are, in a namespace this chart does not own.
				//
				// Without this the pods carried nothing at all, so a policy in a tenant namespace
				// had no way to name them except by matching every pod.
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						ManagedByLabel: "kube-oci-builder",
						InputHashLabel: shortHash(inputHash),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: spec.ServiceAccountName,
					// No API token unless the spec named an identity: a pod running code from a
					// git repository must not carry the credentials of whatever created it.
					// Suppressing the mount needs no ServiceAccount to exist, so it works in
					// whatever namespace a build lands in.
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
func pushNames(obj *ociv1alpha1.ImageBuild, repo string) string {
	if repo == "" {
		return ""
	}
	// EffectiveTags folds in whatever push.ref carries, so a generator that retags through a
	// kustomize images transformer reaches the build the same way it reaches a composition. An
	// invalid ref is caught during resolution, before a Job exists, so it cannot arrive here.
	tags, err := recon.EffectiveTags(obj.Spec.Push.GetTags(), obj.Spec.Push.GetRef())
	if err != nil || len(tags) == 0 {
		return repo
	}
	names := make([]string, 0, len(tags))
	for _, tag := range tags {
		names = append(names, repo+":"+tag)
	}
	return strings.Join(names, ",")
}

// cacheRefFor returns where this object's build cache lives, or "" when caching is disabled.
//
// Always per-object; nothing shares one. See the doc on BuildCache.Ref for why.
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
