package buildcontroller

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// testHash stands in for an input hash wherever the value itself does not matter.
var testHash = "sha256:" + strings.Repeat("a", 64)

// sampleRepo is where the test fixtures publish. Passed explicitly now that the repository is
// RESOLVED -- from the spec, or from the operator's default when the spec names none -- rather than
// read straight off the object.
const sampleRepo = "ghcr.io/me/app"

func sampleBuild() *ociv1alpha1.ImageBuild {
	return &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
		Spec: ociv1alpha1.ImageBuildSpec{
			Context:    ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "src"},
			Dockerfile: "Dockerfile",
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
	}
}

// TestJobNameIsDeterministic — this is what makes a brief two-leader window harmless and lets a
// restarted controller adopt the Job it left running instead of starting a second build.
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

// TestBuildJobRunsRootless is a security assertion, not a configuration one.
//
// ADR 0001 named "a privileged or rootless-BuildKit pod" as the blast radius that justified
// refusing to build at all. Rootless is the half of that this project accepts; privileged is not
// offered at any setting, so nothing in the spec can reach these fields.
func TestBuildJobRunsRootless(t *testing.T) {
	job := buildJob(sampleBuild(), testHash, "https://example/ctx.tgz", sampleConfig(), sampleRepo, true)

	pod := job.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("want one build container, got %d", len(pod.Containers))
	}

	// Every container, not just the build one — the init container fetches the context and has no
	// more reason to be privileged than the build does.
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

		// Asserted in the direction that looks wrong: escalation must be PERMITTED. Setuid
		// newuidmap is how rootless maps a UID range, and NO_NEW_PRIVS makes the kernel ignore
		// the setuid bit, so tightening this stops buildkitd starting at all (ADR 0027). The
		// previous version of this test demanded false, passed every run, and was wrong about the
		// only environment that mattered.
		if sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
			t.Errorf("%s forbids privilege escalation; rootless BuildKit cannot map UIDs and will "+
				"not start", c.Name)
		}

		// With escalation permitted the bounding set is the only thing left holding the line, so
		// it has to be exact rather than merely present.
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
	// Seccomp and AppArmor must be unconfined, and that is not a loosening to tidy away later:
	// rootless BuildKit creates user namespaces and mounts inside them, and both defaults block
	// it. Tightening these makes every build fail, so the assertion is here to say so.
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
	job := buildJob(obj, testHash, "https://example/ctx.tgz", sampleConfig(), sampleRepo, true)

	if got := job.Spec.Template.Spec.ServiceAccountName; got != "builder" {
		t.Errorf("service account = %q, want %q", got, "builder")
	}
}

// TestBuildJobArgs — the argv is the contract with BuildKit, and the pieces that matter are the
// ones that determine the output: platforms, the push target, and the reproducibility levers.
func TestBuildJobArgs(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Platforms = []string{"linux/amd64", "linux/arm64"}
	obj.Spec.Target = "runtime"
	obj.Spec.Args = []ociv1alpha1.BuildArg{{Name: "VERSION", Value: "1.2.3"}}

	job := buildJob(obj, testHash, "https://example/ctx.tgz", sampleConfig(), sampleRepo, true)
	argv := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")

	for _, want := range []string{
		"platform=linux/amd64,linux/arm64",
		"target=runtime",
		"build-arg:VERSION=1.2.3",
		"ghcr.io/me/app:v1",
		"push=true",
		"rewrite-timestamp=true",
		"SOURCE_DATE_EPOCH=0",
		"--metadata-file",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv is missing %q\ngot: %s", want, argv)
		}
	}
}

// TestNetworkNoneIsPassedThrough — the only mode approaching the composer's guarantee.
func TestNetworkNoneIsPassedThrough(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Network = "None"
	job := buildJob(obj, testHash, "https://example/ctx.tgz", sampleConfig(), sampleRepo, true)

	argv := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(argv, "no-network=true") {
		t.Errorf("network: None was not passed to the builder\ngot: %s", argv)
	}
}

// TestCacheRefIsPerObject — nothing may share a cache, so the default must be scoped by namespace
// and name.
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

// TestSecretsAreMountedNotInlined — a credential passed as a build arg lands in the image's
// history; BuildKit's secret mount is the only safe route, and the argv must use it.
func TestSecretsAreMountedNotInlined(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Secrets = []ociv1alpha1.BuildSecret{{
		ID:        "npmrc",
		SecretRef: &ociv1alpha1.LocalObjectReference{Name: "npm-creds"},
	}}

	job := buildJob(obj, testHash, "https://example/ctx.tgz", sampleConfig(), sampleRepo, true)
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

// TestFailureBackoffIsCapped — a build waiting on a human pushing a Dockerfile fix must keep
// checking at a sane interval rather than backing off for hours.
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

// TestInsecureRegistryIsOptInPerHost — naming one internal registry must not downgrade every other
// push the same controller makes, so the attribute appears only when the push host matches.
func TestInsecureRegistryIsOptInPerHost(t *testing.T) {
	cfg := sampleConfig()
	cfg.InsecureRegistries = []string{"registry.internal:5000"}

	secure := buildJob(sampleBuild(), testHash, "https://example/ctx.tgz", cfg, sampleRepo, true)
	if argv := strings.Join(secure.Spec.Template.Spec.Containers[0].Args, " "); strings.Contains(argv, "registry.insecure") {
		t.Errorf("a non-listed host was pushed insecurely\ngot: %s", argv)
	}

	obj := sampleBuild()
	obj.Spec.Push.Repository = "registry.internal:5000/team/app"
	listed := buildJob(obj, testHash, "https://example/ctx.tgz", cfg, obj.Spec.Push.Repository, true)
	if argv := strings.Join(listed.Spec.Template.Spec.Containers[0].Args, " "); !strings.Contains(argv, "registry.insecure=true") {
		t.Errorf("a listed host was not allowed plain HTTP\ngot: %s", argv)
	}
}

// TestInsecureRegistryIsNotInTheInputHash — how the bytes are transported does not change what
// they are, so flipping this must not rebuild every object in the cluster.
func TestInsecureRegistryIsNotInTheInputHash(t *testing.T) {
	obj := sampleBuild()
	obj.Spec.Push.Repository = "registry.internal:5000/team/app"

	plain := sampleConfig()
	insecure := sampleConfig()
	insecure.InsecureRegistries = []string{"registry.internal:5000"}

	// The Job name is derived from the input hash, so identical names prove the hash did not move.
	a := buildJob(obj, testHash, "https://example/ctx.tgz", plain, obj.Spec.Push.Repository, true)
	b := buildJob(obj, testHash, "https://example/ctx.tgz", insecure, obj.Spec.Push.Repository, true)
	if a.Name != b.Name {
		t.Errorf("the insecure list moved the input hash: %q vs %q", a.Name, b.Name)
	}
}

// TestFetchContextUnwrapsTheSourceControllerDirectory runs the init container's script for real,
// against both archive shapes, because the bug it guards was invisible to every unit test: the
// controller-side check strips the wrapper directory and the pod-side extraction did not, so an
// unpinned FROM was correctly refused while every build that PASSED that check then died inside
// BuildKit with "failed to read dockerfile". Only an end-to-end run could see the disagreement.
//
// Skipped where there is no POSIX shell, which is most Windows machines; CI runs it.
func TestFetchContextUnwrapsTheSourceControllerDirectory(t *testing.T) {
	// Skipping encodes a platform fact, not a hope about CI: "CI covers this" is an assertion the
	// test cannot make, and if wget ever left the runner image this would skip green forever while
	// the bug it guards went untested again.
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell; the script is exercised on Linux")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal("no sh; the script that ships in the pod cannot be exercised")
	}
	for _, bin := range []string{"tar", "wget"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("no %s; the script calls it, and substituting it would test a paraphrase", bin)
		}
	}

	for _, tc := range []struct {
		name   string
		prefix string // the wrapper directory, or "" for files at the archive root
	}{
		{"wrapped by source-controller", "src-abc123/"},
		{"already at the root", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Over HTTP, because that is what wget does in the pod.
			srv := contextServer(t, contextTarball(t, tc.prefix, pinnedFrom))

			workspace := filepath.Join(t.TempDir(), "workspace")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatalf("creating workspace: %v", err)
			}

			// contextPath is absolute in the pod; rewrite it to the temp workspace to run here.
			script := strings.ReplaceAll(fetchContextScript(srv.URL), contextPath, workspace)
			out, err := exec.Command(sh, "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("fetch script failed: %v\n%s", err, out)
			}

			// The Dockerfile must land where buildctl looks for it: the workspace root.
			if _, err := os.Stat(filepath.Join(workspace, "Dockerfile")); err != nil {
				got, _ := os.ReadDir(workspace)
				var names []string
				for _, e := range got {
					names = append(names, e.Name())
				}
				t.Fatalf("Dockerfile is not at the context root, so buildctl cannot read it; workspace holds %v", names)
			}
			if _, err := os.Stat(filepath.Join(workspace, ".staging")); !os.IsNotExist(err) {
				t.Error("the staging directory was left behind, so it becomes part of the build context")
			}
		})
	}
}
