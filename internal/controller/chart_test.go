package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The chart is treated as a testable artifact, not just a pile of templates.
//
// The failure these guard against is drift: a flag renamed in Go, or an RBAC marker changed, while
// the chart keeps rendering the old thing. Nothing catches that until a cluster behaves
// differently from the repository, which is exactly the sort of bug that eats an afternoon.

const chartDir = "../../charts/kube-oci-composer"

// render runs `helm template`, skipping the test if helm is unavailable rather than failing —
// a missing local tool is not a defect in the code under test.
func render(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; skipping chart render")
	}

	base := []string{"template", "test-release", chartDir, "--namespace", "oci-composer"}
	out, err := exec.Command("helm", append(base, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// renderExpectingFailure returns helm's output when the render is supposed to fail.
func renderExpectingFailure(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; skipping chart render")
	}

	base := []string{"template", "test-release", chartDir, "--namespace", "oci-composer"}
	out, err := exec.Command("helm", append(base, args...)...).CombinedOutput()
	if err == nil {
		t.Fatalf("expected the render to fail, but it succeeded:\n%s", out)
	}
	return string(out)
}

// TestChartRBACMatchesTheGeneratedRole is the drift guard that matters most.
//
// The chart hand-writes its RBAC so it can be namespaced and templated, which means it can quietly
// diverge from the kubebuilder markers. Too few verbs and the controller fails at runtime in a way
// that looks like a bug; too many and it silently holds permissions nobody reviewed.
func TestChartRBACMatchesTheGeneratedRole(t *testing.T) {
	generated, err := readClusterRole(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatalf("reading generated role: %v", err)
	}

	out := render(t)
	chart := clusterRoleFromRender(t, out, "test-release-kube-oci-composer")

	// Leader-election rules live in a namespaced Role in the chart, so exclude them from the
	// cluster-scoped comparison.
	generatedRules := rulesExcluding(generated, "coordination.k8s.io")

	want := ruleSet(generatedRules)
	got := ruleSet(chart.Rules)

	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("chart ClusterRole is MISSING a rule the controller needs: %s", k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("chart ClusterRole grants a rule the controller does not use: %s", k)
		}
	}
}

// TestChartNeverGrantsSecretListOrWatch — reading one referenced push credential must not turn
// into permission to watch every Secret in the cluster.
func TestChartNeverGrantsSecretListOrWatch(t *testing.T) {
	out := render(t)
	chart := clusterRoleFromRender(t, out, "test-release-kube-oci-composer")

	for _, rule := range chart.Rules {
		if !containsString(rule.APIGroups, "") || !containsString(rule.Resources, "secrets") {
			continue
		}
		for _, verb := range rule.Verbs {
			if verb == "list" || verb == "watch" || verb == "*" {
				t.Fatalf("chart grants %q on secrets; it must be get only", verb)
			}
		}
	}
}

// TestChartFlagsMatchTheBinary — every flag the chart renders must exist, or the container
// crash-loops on an unknown flag with no other warning.
func TestChartFlagsMatchTheBinary(t *testing.T) {
	out := render(t,
		"--set", "operator.servingHost=oci.test",
		"--set", "operator.s3.endpoint=https://s3.test",
		"--set", "operator.s3.bucket=artifacts",
		"--set", "operator.s3.prefix=composer",
		"--set", "operator.s3.presignBlobs=true",
		"--set", "operator.gc.dryRun=true",
	)

	known := knownFlags(t, "../../cmd/oci-composer")

	// Scoped to the COMPOSER's container. One chart renders both controllers, so scanning the whole
	// document would check the builder's flags against the composer's binary.
	var seen int
	for _, line := range strings.Split(containerArgs(t, out, "test-release-kube-oci-composer"), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- --") {
			continue
		}
		flag := strings.TrimPrefix(line, "- --")
		if i := strings.Index(flag, "="); i >= 0 {
			flag = flag[:i]
		}
		if _, ok := known[flag]; !ok {
			t.Errorf("chart renders --%s, which the binary does not define", flag)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no flags found in the rendered output; the assertion proves nothing")
	}
}

// TestChartRejectsIncoherentValues — misconfiguration should fail at template time, where the
// message can be specific, rather than at container start.
func TestChartRejectsIncoherentValues(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"s3 backend without an endpoint": {
			[]string{"--set", "operator.servingHost=oci.test", "--set", "operator.storage.backend=s3"},
			"requires operator.s3.endpoint",
		},
		"presign without an endpoint": {
			[]string{"--set", "operator.servingHost=oci.test", "--set", "operator.s3.presignBlobs=true"},
			"requires operator.s3.endpoint",
		},
		"endpoint without a bucket": {
			[]string{"--set", "operator.servingHost=oci.test", "--set", "operator.s3.endpoint=https://s3.test"},
			"requires operator.s3.bucket",
		},
		"ingress without a host": {
			[]string{"--set", "operator.servingHost=oci.test", "--set", "ingress.enabled=true"},
			"requires ingress.host",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out := renderExpectingFailure(t, tc.args...)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("error does not mention %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestChartUsesRecreateStrategy — a rolling update would briefly run two pods contending for a
// ReadWriteOnce volume, and the new one would not be the leader so it would serve nothing.
func TestChartUsesRecreateStrategy(t *testing.T) {
	out := render(t, "--set", "operator.servingHost=oci.test")
	if !strings.Contains(out, "type: Recreate") {
		t.Fatal("deployment does not use the Recreate strategy")
	}
}

// TestChartMountsWritablePaths — the root filesystem is read-only, so assembly's temp directory
// and both storage directories must be explicitly writable or every build fails.
func TestChartMountsWritablePaths(t *testing.T) {
	out := render(t, "--set", "operator.servingHost=oci.test")
	for _, path := range []string{"/tmp", "/var/lib/oci-composer", "/var/cache/oci-composer"} {
		if !strings.Contains(out, "mountPath: "+path) {
			t.Errorf("no writable mount for %s, but the root filesystem is read-only", path)
		}
	}
	if !strings.Contains(out, "readOnlyRootFilesystem: true") {
		t.Error("root filesystem is not read-only")
	}
}

// TestChartServiceExposesOnlyTheOCIPort — metrics and probes must not land on the public listener.
func TestChartServiceExposesOnlyTheOCIPort(t *testing.T) {
	out := render(t, "--set", "operator.servingHost=oci.test",
		"--set", "ingress.enabled=true", "--set", "ingress.host=oci.test")

	if !strings.Contains(out, "path: /v2/") {
		t.Error("ingress does not narrow to /v2/, so probes and metrics would be publicly exposed")
	}
}

// TestChartCredentialsAreNotFlags — a credential in argv is visible in ps, in the pod spec, and in
// every kubectl describe.
func TestChartCredentialsAreNotFlags(t *testing.T) {
	out := render(t,
		"--set", "operator.servingHost=oci.test",
		"--set", "operator.s3.endpoint=https://s3.test",
		"--set", "operator.s3.bucket=artifacts",
		"--set", "operator.s3.existingSecret=composer-s3-creds",
	)
	if strings.Contains(out, "--s3-access-key") || strings.Contains(out, "--s3-secret") {
		t.Fatal("credentials are being passed as flags")
	}
	if !strings.Contains(out, "AWS_ACCESS_KEY_ID") || !strings.Contains(out, "secretKeyRef") {
		t.Fatal("credentials are not injected from a Secret")
	}
}

// --- helpers ---

func readClusterRole(path string) (*rbacv1.ClusterRole, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, err
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		return nil, err
	}
	return &role, nil
}

func clusterRoleFromRender(t *testing.T, out, name string) *rbacv1.ClusterRole {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---") {
		var probe struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			continue
		}
		if probe.Kind != "ClusterRole" || probe.Metadata.Name != name {
			continue
		}
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			t.Fatalf("parsing ClusterRole: %v", err)
		}
		return &role
	}
	t.Fatalf("no ClusterRole named %q in the rendered output", name)
	return nil
}

func rulesExcluding(role *rbacv1.ClusterRole, group string) []rbacv1.PolicyRule {
	var out []rbacv1.PolicyRule
	for _, r := range role.Rules {
		if containsString(r.APIGroups, group) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ruleSet flattens rules into comparable "group/resource:verb" strings, so a rule split across
// two entries compares equal to the same permissions expressed as one.
func ruleSet(rules []rbacv1.PolicyRule) map[string]struct{} {
	out := make(map[string]struct{})
	for _, r := range rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					out[g+"/"+res+":"+v] = struct{}{}
				}
			}
		}
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// knownFlags asks the binary itself which flags it defines, rather than keeping a list here that
// would need updating in lockstep — the very drift these tests exist to catch.
// knownFlags lists the flags a binary defines, so a chart cannot render one that does not exist.
func knownFlags(t *testing.T, cmdPath string) map[string]struct{} {
	t.Helper()

	cmd := exec.Command("go", "run", cmdPath, "-h")
	out, _ := cmd.CombinedOutput() // flag.Usage exits non-zero; the output is what matters

	flags := make(map[string]struct{})
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-") {
			continue
		}
		name := strings.TrimLeft(line, "-")
		for _, cut := range []string{" ", "\t", "="} {
			if i := strings.Index(name, cut); i >= 0 {
				name = name[:i]
			}
		}
		if name != "" {
			flags[name] = struct{}{}
		}
	}
	if len(flags) == 0 {
		t.Fatalf("could not determine the binary's flags:\n%s", out)
	}
	return flags
}

// TestChartRefusesSharedStorageOnEmptyDir — "shared" is an assertion the chart cannot verify, so
// the one combination that provably contradicts it has to be refused at template time.
//
// replicaCount>1 with storage.shared=true and persistence.enabled=false gives every pod its own
// emptyDir while telling the operator they are shared. Each replica then restores nothing, reports
// ready anyway (readiness is observed on attempt, deliberately), joins the Service, and 404s every
// pull routed to it. That is worse than not serving: it is intermittent, and it looks like the
// registry losing images.
func TestChartRefusesSharedStorageOnEmptyDir(t *testing.T) {
	out := renderExpectingFailure(t,
		"--set", "replicaCount=2",
		"--set", "operator.storage.shared=true",
		"--set", "persistence.enabled=false",
		"--set", "operator.servingHost=oci.test")

	if !strings.Contains(out, "its own emptyDir") {
		t.Errorf("the failure does not explain that the replicas would not share anything:\n%s", out)
	}
}

// The same shape must still render once storage is genuinely shared, or the guard is just a ban on
// running more than one replica.
func TestChartAllowsSharedStorageWithAVolume(t *testing.T) {
	render(t,
		"--set", "replicaCount=2",
		"--set", "operator.storage.shared=true",
		"--set", "persistence.enabled=true",
		"--set", "operator.servingHost=oci.test")

	// s3 is shared by construction, so it needs no volume.
	render(t,
		"--set", "replicaCount=2",
		"--set", "operator.storage.backend=s3",
		"--set", "operator.s3.endpoint=https://s3.test",
		"--set", "operator.s3.bucket=blobs",
		"--set", "operator.servingHost=oci.test")
}

// TestEachServiceSelectsOnlyItsOwnComponent — one chart now deploys three workloads into one
// namespace, and this is the bug that shipped when the registry was added.
//
// Every pod carried the chart's two selector labels and nothing else, so the composer's Service
// selected the registry pod as well: a pull routed to the wrong container, from a Service that
// reports itself perfectly healthy. Harmless while the registry was off by default; shipping it on
// would have made it everyone's default.
func TestEachServiceSelectsOnlyItsOwnComponent(t *testing.T) {
	out := render(t)

	pods := podLabelsByWorkload(t, out)
	if len(pods) < 2 {
		t.Fatalf("expected several workloads, found %d; the assertion proves nothing", len(pods))
	}

	for svc, selector := range serviceSelectors(t, out) {
		var matched []string
		for workload, labels := range pods {
			if selects(selector, labels) {
				matched = append(matched, workload)
			}
		}
		if len(matched) != 1 {
			t.Errorf("Service %s selects %v. A selector that matches more than one workload sends "+
				"traffic to whichever pod answers first, and looks healthy while doing it.",
				svc, matched)
		}
	}
}

// TestBothControllersAreWiredToTheDefaultRegistry — the point of bundling one. If either controller
// misses the flags, objects that name no repository sit Pending forever with a message about
// configuration rather than about themselves.
func TestBothControllersAreWiredToTheDefaultRegistry(t *testing.T) {
	out := render(t)

	for _, deployment := range []string{
		"test-release-kube-oci-composer",
		"test-release-kube-oci-composer-builder",
	} {
		args := containerArgs(t, out, deployment)
		for _, want := range []string{"--default-registry=", "--default-push-secret=", "--insecure-registry="} {
			if !strings.Contains(args, want) {
				t.Errorf("%s does not render %s; objects using the default registry would not "+
					"publish\nargs:\n%s", deployment, want, args)
			}
		}
	}
}

// TestTheGeneratedCredentialMatchesTheRegistrysHtpasswd — two Secrets rendered from one password.
// If they drift, every push is rejected by a registry the chart itself installed.
func TestTheGeneratedCredentialMatchesTheRegistrysHtpasswd(t *testing.T) {
	out := render(t)

	push := documentNamed(t, out, "Secret", "test-release-kube-oci-composer-registry-push")
	htpasswd := documentNamed(t, out, "Secret", "test-release-kube-oci-composer-registry-htpasswd")

	user := "composer"
	if !strings.Contains(htpasswd, user+":$2a$") {
		t.Errorf("the htpasswd entry is not a bcrypt hash for %q:\n%s", user, htpasswd)
	}
	if !strings.Contains(push, `\"username\":\"`+user+`\"`) && !strings.Contains(push, `"username":"`+user+`"`) {
		t.Errorf("the push credential is not for %q:\n%s", user, push)
	}
	// The registry must actually require it, or the generated credential is decoration and the
	// write path is open to anything that can reach the Service.
	config := documentNamed(t, out, "ConfigMap", "test-release-kube-oci-composer-registry")
	for _, want := range []string{"htpasswd", "anonymousPolicy", `"read"`} {
		if !strings.Contains(config, want) {
			t.Errorf("the registry config does not mention %s; anonymous-read/authenticated-write "+
				"is not actually configured", want)
		}
	}
}

// podLabelsByWorkload returns each Deployment's pod-template labels, keyed by workload name.
func podLabelsByWorkload(t *testing.T, rendered string) map[string]map[string]string {
	t.Helper()

	out := map[string]map[string]string{}
	for _, doc := range strings.Split(rendered, "\n---\n") {
		var d struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil || d.Kind != "Deployment" {
			continue
		}
		out[d.Metadata.Name] = d.Spec.Template.Metadata.Labels
	}
	return out
}

// serviceSelectors returns each Service's selector, keyed by Service name.
func serviceSelectors(t *testing.T, rendered string) map[string]map[string]string {
	t.Helper()

	out := map[string]map[string]string{}
	for _, doc := range strings.Split(rendered, "\n---\n") {
		var d struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Selector map[string]string `json:"selector"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil || d.Kind != "Service" {
			continue
		}
		if len(d.Spec.Selector) > 0 {
			out[d.Metadata.Name] = d.Spec.Selector
		}
	}
	return out
}

// selects reports whether a Service selector matches a set of pod labels, by Kubernetes' rule: every
// selector entry must be present and equal.
func selects(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// documentNamed returns the single rendered document of a kind and name.
func documentNamed(t *testing.T, rendered, kind, name string) string {
	t.Helper()

	for _, doc := range strings.Split(rendered, "\n---\n") {
		var d struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			continue
		}
		if d.Kind == kind && d.Metadata.Name == name {
			return doc
		}
	}
	t.Fatalf("no %s named %s in the rendered output", kind, name)
	return ""
}
