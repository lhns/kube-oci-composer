package buildcontroller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// testHash stands in for an input hash wherever the value itself does not matter.
var testHash = "sha256:" + strings.Repeat("a", 64)

// sampleRepo is where the test fixtures publish, passed explicitly because the repository is
// resolved (spec or operator default), not read off the object.
const sampleRepo = "ghcr.io/me/app"

func sampleBuild() *ociv1alpha1.ImageBuild {
	return &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
		Spec: ociv1alpha1.ImageBuildSpec{
			Context:    &ociv1alpha1.BuildContext{SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "src"}},
			Dockerfile: &ociv1alpha1.DockerfileSource{Path: "Dockerfile"},
			Platforms:  []string{"linux/amd64"},
			Push: &ociv1alpha1.Push{
				Repository: "ghcr.io/me/app",
				Tags:       []string{"v1"},
			},
		},
	}
}

func sampleConfig() JobConfig {
	return JobConfig{
		BuilderImage:    "moby/buildkit:rootless@sha256:" + strings.Repeat("a", 64),
		FrontendImage:   "docker/dockerfile:1@sha256:" + strings.Repeat("b", 64),
		SourceDateEpoch: "0",
		FetcherImage:    "ghcr.io/lhns/kube-oci-builder@sha256:" + strings.Repeat("c", 64),
	}
}

// TestJobNameIsDeterministic, so a second leader or a restarted controller adopts the existing Job.
func TestJobNameIsDeterministic(t *testing.T) {
	obj := sampleBuild()
	const hash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	first := jobName(obj, hash)
	if second := jobName(obj, hash); first != second {
		t.Fatalf("not deterministic: %q then %q", first, second)
	}
	if other := jobName(obj, "sha256:ffff"+strings.Repeat("0", 60)); other == first {
		t.Error("different inputs produced the same job name; a stale build would be adopted")
	}
	if len(first) > 63 {
		t.Errorf("name %q is %d chars, over the 63 limit", first, len(first))
	}
}

// TestJobNameStaysWithinLimit — a long object name must not produce an invalid Job name.
func TestJobNameStaysWithinLimit(t *testing.T) {
	obj := sampleBuild()
	obj.Name = strings.Repeat("x", 200)
	name := jobName(obj, testHash)
	if len(name) > 63 {
		t.Errorf("name is %d chars, over the limit", len(name))
	}
}

// TestBuildJobRunsRootless is a security assertion: never privileged (ADR 0001), and exactly the
// rootless posture ADR 0027 requires.
func TestBuildJobRunsRootless(t *testing.T) {
	job := buildJob(sampleBuild(), testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo, "", "", "", "", true)

	pod := job.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("want one build container, got %d", len(pod.Containers))
	}

	// Every container, including the context fetcher.
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		sc := c.SecurityContext
		if sc == nil {
			t.Errorf("%s has no security context", c.Name)
			continue
		}
		if sc.Privileged == nil || *sc.Privileged {
			t.Errorf("%s is privileged", c.Name)
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("%s may run as root", c.Name)
		}
		if sc.RunAsUser == nil || *sc.RunAsUser == 0 {
			t.Errorf("%s does not pin a non-zero uid", c.Name)
		}

		// Escalation must be permitted: setuid newuidmap maps the UID range, and NO_NEW_PRIVS
		// would stop buildkitd from starting (ADR 0027).
		if sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
			t.Errorf("%s forbids privilege escalation; rootless BuildKit cannot map UIDs and will "+
				"not start", c.Name)
		}

		// With escalation permitted, the capability set must be exact.
		caps := sc.Capabilities
		if caps == nil || len(caps.Drop) != 1 || caps.Drop[0] != "ALL" {
			t.Errorf("%s does not drop ALL capabilities: %+v", c.Name, caps)
			continue
		}
		want := map[corev1.Capability]bool{"SETUID": true, "SETGID": true}
		for _, add := range caps.Add {
			if !want[add] {
				t.Errorf("%s adds capability %q, which the UID mapping does not need", c.Name, add)
			}
			delete(want, add)
		}
		for missing := range want {
			t.Errorf("%s is missing %q; rootless BuildKit cannot map UIDs without it", c.Name, missing)
		}
	}
	// Unconfined is required: rootless BuildKit creates user namespaces and mounts inside them.
	build := pod.Containers[0].SecurityContext
	if build.SeccompProfile == nil || build.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Errorf("seccomp = %+v, want Unconfined; rootless BuildKit cannot run otherwise",
			build.SeccompProfile)
	}
	if build.AppArmorProfile == nil || build.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined {
		t.Errorf("apparmor = %+v, want Unconfined; rootless BuildKit cannot run otherwise",
			build.AppArmorProfile)
	}

	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy is %q; a retried RUN just delays the failure", pod.RestartPolicy)
	}
}

// TestBuildJobUsesTheObjectsServiceAccount — the build pod runs as spec.serviceAccountName, never
// the controller's.
func TestBuildJobUsesTheObjectsServiceAccount(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.ServiceAccountName = "builder"
	job := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo, "", "", "", "", true)

	if got := job.Spec.Template.Spec.ServiceAccountName; got != "builder" {
		t.Errorf("service account = %q, want %q", got, "builder")
	}
}

// TestBuildJobArgs pins the parts of the BuildKit argv that determine the output.
func TestBuildJobArgs(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Platforms = []string{"linux/amd64", "linux/arm64"}
	obj.Spec.Target = "runtime"
	obj.Spec.Args = []ociv1alpha1.BuildArg{{Name: "VERSION", Value: "1.2.3"}}

	job := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo, "", "", "", "", true)
	argv := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")

	for _, want := range []string{
		"platform=linux/amd64,linux/arm64",
		"target=runtime",
		"build-arg:VERSION=1.2.3",
		// Pushed by digest; the controller tags afterwards (ADR 0054).
		"name=ghcr.io/me/app,push=true,push-by-digest=true",
		"push=true",
		"rewrite-timestamp=true",
		"SOURCE_DATE_EPOCH=0",
		"--metadata-file",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv is missing %q\ngot: %s", want, argv)
		}
	}
	// The Job names no tag.
	if strings.Contains(argv, "ghcr.io/me/app:") {
		t.Errorf("the build Job names a tag; naming belongs to the controller\ngot: %s", argv)
	}
}

// TestNetworkNoneIsPassedThrough — the only mode approaching the composer's guarantee.
func TestNetworkNoneIsPassedThrough(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Network = "None"
	job := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo, "", "", "", "", true)

	argv := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(argv, "no-network=true") {
		t.Errorf("network: None was not passed to the builder\ngot: %s", argv)
	}
}

// TestCacheRefIsPerObject: the default cache ref is scoped by namespace and name.
func TestCacheRefIsPerObject(t *testing.T) {
	a := sampleBuild()
	b := sampleBuild()
	b.Namespace, b.Name = "team-b", "other"

	refA, refB := cacheRefFor(a, sampleRepo), cacheRefFor(b, sampleRepo)
	if refA == refB {
		t.Errorf("two objects share cache ref %q", refA)
	}
	if !strings.Contains(refA, a.Namespace) || !strings.Contains(refA, a.Name) {
		t.Errorf("cache ref %q is not scoped to the object", refA)
	}

	disabled := sampleBuild()
	disabled.Spec.Cache = &ociv1alpha1.BuildCache{Mode: "Disabled"}
	if cacheRefFor(disabled, sampleRepo) != "" {
		t.Error("cache: Disabled still exported a cache")
	}
}

// TestSecretsAreMountedNotInlined: a build arg lands in the image history; only a secret mount is
// safe.
func TestSecretsAreMountedNotInlined(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Secrets = []ociv1alpha1.BuildSecret{{
		ID:        "npmrc",
		SecretRef: &ociv1alpha1.LocalObjectReference{Name: "npm-creds"},
	}}

	job := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo, "", "", "", "", true)
	argv := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")

	if !strings.Contains(argv, "--secret id=npmrc") {
		t.Errorf("the secret was not passed as a secret mount\ngot: %s", argv)
	}
	if strings.Contains(argv, "build-arg:npmrc") {
		t.Error("the secret was passed as a build arg, which would land in the image history")
	}

	var mounted bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "npm-creds" {
			mounted = true
		}
	}
	if !mounted {
		t.Error("the Secret is not projected into the pod")
	}
}

// TestFailureBackoffIsCapped, so a pushed fix is noticed promptly.
func TestFailureBackoffIsCapped(t *testing.T) {
	if got := failureBackoff(0); got != pendingRetryInterval {
		t.Errorf("first retry is %v, want %v", got, pendingRetryInterval)
	}
	if failureBackoff(1) <= failureBackoff(0) {
		t.Error("backoff does not grow")
	}
	if got := failureBackoff(50); got != maxFailureBackoff {
		t.Errorf("backoff after many failures is %v, want the %v cap", got, maxFailureBackoff)
	}
}

// TestInsecureRegistryIsOptInPerHost: plain HTTP only for a listed push host.
func TestInsecureRegistryIsOptInPerHost(t *testing.T) {
	cfg := sampleConfig()
	cfg.InsecureRegistries = []string{"registry.internal:5000"}

	secure := buildJob(sampleBuild(), testHash, "https://example/ctx.tgz", "sha256:ctx", cfg, sampleRepo, "", "", "", "", true)
	if argv := strings.Join(secure.Spec.Template.Spec.Containers[0].Args, " "); strings.Contains(argv, "registry.insecure") {
		t.Errorf("a non-listed host was pushed insecurely\ngot: %s", argv)
	}

	obj := sampleBuild()
	obj.Spec.Push.Repository = "registry.internal:5000/team/app"
	listed := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", cfg, obj.Spec.Push.Repository, "", "", "", "", true)
	if argv := strings.Join(listed.Spec.Template.Spec.Containers[0].Args, " "); !strings.Contains(argv, "registry.insecure=true") {
		t.Errorf("a listed host was not allowed plain HTTP\ngot: %s", argv)
	}
}

// TestInsecureRegistryIsNotInTheInputHash: transport, not content, so flipping it must not rebuild.
func TestInsecureRegistryIsNotInTheInputHash(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Push.Repository = "registry.internal:5000/team/app"

	plain := sampleConfig()
	insecure := sampleConfig()
	insecure.InsecureRegistries = []string{"registry.internal:5000"}

	// The Job name is derived from the input hash, so identical names prove the hash did not move.
	a := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", plain, obj.Spec.Push.Repository, "", "", "", "", true)
	b := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", insecure, obj.Spec.Push.Repository, "", "", "", "", true)
	if a.Name != b.Name {
		t.Errorf("the insecure list moved the input hash: %q vs %q", a.Name, b.Name)
	}
}

// TestTheFetcherIsToldWhatToFetch pins the argv contract with internal/fetchcontext, which is
// tested against real archives there.
func TestTheFetcherIsToldWhatToFetch(t *testing.T) {
	t.Run("a Flux artifact", func(t *testing.T) {
		obj := sampleBuild()
		obj.Spec.Context.SourceRef.Subpath = "ui"
		job := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(),
			sampleRepo, "", "", "", "", true)

		init := job.Spec.Template.Spec.InitContainers
		if len(init) != 1 {
			t.Fatalf("want one init container, got %d", len(init))
		}
		if init[0].Image != sampleConfig().FetcherImage {
			t.Errorf("the fetcher runs %q, not the fetcher image", init[0].Image)
		}
		args := strings.Join(init[0].Args, " ")
		for _, want := range []string{
			"fetch-context", "--kind=sourceRef", "--url=https://example/ctx.tgz",
			// The digest is verified for a Flux artifact too.
			"--digest=sha256:ctx", "--subpath=ui",
		} {
			if !strings.Contains(args, want) {
				t.Errorf("fetcher args are missing %q; got: %s", want, args)
			}
		}
	})

	t.Run("a fetched archive", func(t *testing.T) {
		obj := sampleBuild()
		obj.Spec.Context = &ociv1alpha1.BuildContext{Fetch: &ociv1alpha1.FetchSource{
			URL: "https://example/app.tgz", Digest: "sha256:decl", Unpack: "tar.gz", Subpath: "app-1.2.3",
		}}
		job := buildJob(obj, testHash, "https://example/app.tgz", "sha256:decl", sampleConfig(),
			sampleRepo, "", "", "", "", true)

		args := strings.Join(job.Spec.Template.Spec.InitContainers[0].Args, " ")
		for _, want := range []string{"--kind=fetch", "--digest=sha256:decl", "--unpack=tar.gz",
			"--subpath=app-1.2.3"} {
			if !strings.Contains(args, want) {
				t.Errorf("fetcher args are missing %q; got: %s", want, args)
			}
		}
	})

	// No context, no fetcher.
	t.Run("no context", func(t *testing.T) {
		obj := sampleBuild()
		obj.Spec.Context = nil
		obj.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{Inline: "FROM scratch\n"}
		job := buildJob(obj, testHash, "", "", sampleConfig(), sampleRepo, "", "", "df", "", true)

		if got := len(job.Spec.Template.Spec.InitContainers); got != 0 {
			t.Errorf("a context-less build runs %d init containers", got)
		}
	})
}

// TestBuildPodsAreSelectable: pod labels (not only Job labels) let NetworkPolicies, quotas and
// admission rules in tenant namespaces select build pods as a class.
func TestBuildPodsAreSelectable(t *testing.T) {
	job := buildJob(sampleBuild(), testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo, "", "", "", "", true)

	labels := job.Spec.Template.Labels
	if labels == nil {
		t.Fatal("build pods carry no labels; nothing outside this namespace can select them")
	}
	if got := labels[ManagedByLabel]; got != "kube-oci-builder" {
		t.Errorf("%s = %q, want kube-oci-builder", ManagedByLabel, got)
	}
	if labels[InputHashLabel] == "" {
		t.Errorf("%s is empty; a pod cannot be tied back to the build that made it", InputHashLabel)
	}

	// The controller finds Jobs by their own labels, so those must stay.
	if got := job.Labels[ManagedByLabel]; got != "kube-oci-builder" {
		t.Errorf("the Job lost its own %s label: %q", ManagedByLabel, got)
	}
}

// TestTheBuildTrustsTheRegistryCA: the volume, writable bundle, env var and script merge are gated
// on one condition in Go, so they can be asserted on the rendered container.
func TestTheBuildTrustsTheRegistryCA(t *testing.T) {
	job := buildJob(sampleBuild(), testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo,
		"", "build-registry-ca", "", "", true)
	pod := job.Spec.Template.Spec
	container := pod.Containers[0]

	var haveCA, haveBundle bool
	for _, v := range pod.Volumes {
		switch v.Name {
		case "registry-ca":
			haveCA = true
			if v.Secret == nil || v.Secret.SecretName != "build-registry-ca" {
				t.Errorf("the CA volume must come from the copied Secret: %+v", v)
			}
		case "ca-bundle":
			haveBundle = true
			// uid 1000 cannot write to the image's /etc/ssl/certs.
			if v.EmptyDir == nil {
				t.Errorf("the merged bundle needs a writable volume: %+v", v)
			}
		}
	}
	if !haveCA || !haveBundle {
		t.Fatalf("both volumes are required; got %v", pod.Volumes)
	}

	var sslCertFile string
	for _, e := range container.Env {
		if e.Name == "SSL_CERT_FILE" {
			sslCertFile = e.Value
		}
	}
	if sslCertFile != caBundlePath {
		t.Errorf("SSL_CERT_FILE = %q, want the merged bundle %q", sslCertFile, caBundlePath)
	}

	// SSL_CERT_FILE replaces the system pool, so the bundle must include the image's own roots.
	script := container.Command[2]
	if !strings.Contains(script, "ca-certificates.crt") {
		t.Error("the bundle must include the image's own roots, or public registries stop verifying")
	}
	if !strings.Contains(script, caBundlePath) {
		t.Errorf("the script must write the bundle SSL_CERT_FILE names:\n%s", script)
	}
	if !strings.Contains(script, "|| true") {
		t.Error("a builder image with no system bundle must not fail the build under `set -e`")
	}
}

// TestNoCAMeansNoCAPlumbing: without a CA the Job is unchanged.
func TestNoCAMeansNoCAPlumbing(t *testing.T) {
	job := buildJob(sampleBuild(), testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo,
		"", "", "", "", true)
	pod := job.Spec.Template.Spec

	for _, v := range pod.Volumes {
		if v.Name == "registry-ca" || v.Name == "ca-bundle" {
			t.Errorf("no CA is configured, so %q should not be mounted", v.Name)
		}
	}
	for _, e := range pod.Containers[0].Env {
		if e.Name == "SSL_CERT_FILE" {
			t.Errorf("SSL_CERT_FILE must not be set without a CA to point it at: %q", e.Value)
		}
	}
	if strings.Contains(pod.Containers[0].Command[2], "ca-bundle") {
		t.Error("the script must not merge a bundle that does not exist")
	}
}

// TestAContextDockerfileStillComesFromTheContext: `--local dockerfile=` points inside the context.
func TestAContextDockerfileStillComesFromTheContext(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{Path: "build/Dockerfile.prod"}
	job := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo, "", "", "", "", true)
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")

	if !strings.Contains(args, "--local dockerfile=/workspace/build") {
		t.Errorf("the dockerfile local must point inside the context:\n%s", args)
	}
	if !strings.Contains(args, "--opt filename=Dockerfile.prod") {
		t.Errorf("filename must be the base name from the spec:\n%s", args)
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == dockerfileVolume {
			t.Error("a context Dockerfile must not project a volume; it is already in the context")
		}
	}
}

// TestAnInlineDockerfileIsProjectedAsItsOwnLocal: `context` and `dockerfile` are independent
// BuildKit locals.
func TestAnInlineDockerfileIsProjectedAsItsOwnLocal(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{Inline: "FROM scratch\n"}
	job := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo,
		"", "", "app-abc123-dockerfile", "", true)
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")

	if !strings.Contains(args, "--local dockerfile=/dockerfile") {
		t.Errorf("the dockerfile local must be its own mount:\n%s", args)
	}
	if !strings.Contains(args, "--opt filename=Dockerfile") {
		t.Errorf("filename must be the fixed projected name:\n%s", args)
	}
	// The context local is untouched; copying the Dockerfile in could overwrite one there.
	if !strings.Contains(args, "--local context=/workspace") {
		t.Errorf("the context local must be unchanged:\n%s", args)
	}

	var mount *corev1.VolumeMount
	for i, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == dockerfileVolume {
			mount = &job.Spec.Template.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatal("no dockerfile volume mounted, so the local points at an empty directory")
	}
	// subPath gives a plain file, not a Secret volume's ..data symlink farm, which fsutil walks.
	if mount.SubPath != dockerfileName {
		t.Errorf("the dockerfile mount must use subPath, got %q", mount.SubPath)
	}
	if !mount.ReadOnly {
		t.Error("the dockerfile mount must be read-only")
	}
}

// TestTheProjectedDockerfileComesFromTheControllersOwnSecret: projecting the user's object would
// let an edit before pod start bypass the FROM check.
func TestTheProjectedDockerfileComesFromTheControllersOwnSecret(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Dockerfile = &ociv1alpha1.DockerfileSource{Inline: "FROM scratch\n"}
	job := buildJob(obj, testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(), sampleRepo,
		"", "", "app-abc123-dockerfile", "", true)

	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name != dockerfileVolume {
			continue
		}
		if v.ConfigMap != nil {
			t.Fatal("the Dockerfile is projected from a ConfigMap, which the kubelet re-reads at " +
				"pod start; the pod could build bytes the controller never checked")
		}
		if v.Secret == nil || v.Secret.SecretName != "app-abc123-dockerfile" {
			t.Fatalf("expected the controller's own Secret, got %+v", v.VolumeSource)
		}
		return
	}
	t.Fatal("no dockerfile volume rendered")
}
