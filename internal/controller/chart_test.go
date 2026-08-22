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

// installable is the smallest set of values that make the chart render at all.
//
// registry.publish.mode has no default on purpose — the chart refuses to install until an operator
// says how workloads reach the registry, because guessing produces images nothing can pull. Every
// test that is not ABOUT that question wants a valid install, so the helpers supply one. `--set`
// later wins, so a test can still override the mode; `renderRaw` exists for the tests that must
// see the unset case.
var installable = []string{"--set", "registry.publish.mode=internalOnly"}

// render runs `helm template`, skipping the test if helm is unavailable rather than failing —
// a missing local tool is not a defect in the code under test.
func render(t *testing.T, args ...string) string {
	t.Helper()
	return renderRaw(t, append(append([]string{}, installable...), args...)...)
}

// renderRaw is render without the installable defaults, for tests about what the chart REFUSES.
func renderRaw(t *testing.T, args ...string) string {
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
//
// Carries the installable defaults for the same reason render does: a test asserting that the
// chart refuses a thin retention margin must fail for THAT reason, not because it forgot to say
// how workloads reach the registry.
func renderExpectingFailure(t *testing.T, args ...string) string {
	t.Helper()
	return renderRawExpectingFailure(t, append(append([]string{}, installable...), args...)...)
}

// renderRawExpectingFailure is renderExpectingFailure with nothing supplied.
func renderRawExpectingFailure(t *testing.T, args ...string) string {
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
		"--set", "operator.s3.endpoint=https://s3.test",
		"--set", "operator.s3.bucket=artifacts",
		"--set", "operator.s3.prefix=composer",
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
		"endpoint without a bucket": {
			[]string{"--set", "operator.s3.endpoint=https://s3.test"},
			"requires operator.s3.bucket",
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

// TestChartMountsWritablePaths — the root filesystem is read-only, so assembly's temp directory and
// the layer cache must be explicitly writable or every build fails.
func TestChartMountsWritablePaths(t *testing.T) {
	out := render(t)
	for _, path := range []string{"/tmp", "/var/cache/oci-composer"} {
		if !strings.Contains(out, "mountPath: "+path) {
			t.Errorf("no writable mount for %s, but the root filesystem is read-only", path)
		}
	}
	if !strings.Contains(out, "readOnlyRootFilesystem: true") {
		t.Error("root filesystem is not read-only")
	}
}

// TestChartCredentialsAreNotFlags — a credential in argv is visible in ps, in the pod spec, and in
// every kubectl describe.
func TestChartCredentialsAreNotFlags(t *testing.T) {
	out := render(t,
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

// TestMoreThanOneComposerReplicaStillRenders.
//
// What this asserts is now much smaller than its ancestor did, and that is the point: with no blob
// store there is nothing for a second replica to disagree about. It reconciles nothing until it
// wins the lease, and the volume it may or may not have is a layer cache, where a cold start costs
// a refetch rather than a failed pull.
//
// Kept because "extra replicas are harmless" is a claim, and an untested claim about replica counts
// is how the shared-storage guards came to exist in the first place.
func TestMoreThanOneComposerReplicaStillRenders(t *testing.T) {
	render(t,
		"--set", "replicaCount=2",
		"--set", "persistence.enabled=true")

	render(t,
		"--set", "replicaCount=2",
		"--set", "operator.s3.endpoint=https://s3.test",
		"--set", "operator.s3.bucket=blobs")
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
		// StatefulSets too: the registry became one so that clustering could not be a kind switch
		// under an operator's feet (ADR 0039). A helper that looked only at Deployments would have
		// quietly stopped covering it, and this test would have passed by finding no workload at
		// all rather than by finding one.
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil ||
			(d.Kind != "Deployment" && d.Kind != "StatefulSet") {
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

// TestBothControllersAreScrapable.
//
// The builder had no metrics Service at all, so its :8080 was unreachable while the composer's was
// scraped — an asymmetry with no reason behind it, and the kind that survives because nobody
// notices a metric that was never there. The builder is the component that creates Jobs; its
// reconcile errors and queue depth are exactly what you want when builds stop happening.
func TestBothControllersAreScrapable(t *testing.T) {
	out := render(t)

	byComponent := map[string]bool{}
	for svc, selector := range serviceSelectors(t, out) {
		if !strings.HasSuffix(svc, "-metrics") {
			continue
		}
		byComponent[selector["app.kubernetes.io/component"]] = true
	}

	for _, component := range []string{"composer", "builder"} {
		if !byComponent[component] {
			t.Errorf("no metrics Service selects the %s; its metrics are unscrapable", component)
		}
	}
}

// TestATurnedOffComponentExposesNothing — a Service left behind by a disabled component selects no
// pods and reports itself healthy, which is a worse failure than an absent one.
func TestATurnedOffComponentExposesNothing(t *testing.T) {
	for _, tc := range []struct{ toggle, absent string }{
		{"imageBuild.enabled=false", "builder-metrics"},
		{"imageComposition.enabled=false", "composer"},
	} {
		t.Run(tc.toggle, func(t *testing.T) {
			out := render(t, "--set", tc.toggle)
			for svc, selector := range serviceSelectors(t, out) {
				if selector["app.kubernetes.io/component"] == "composer" &&
					tc.toggle == "imageComposition.enabled=false" {
					t.Errorf("Service %s still selects the composer, which is not installed", svc)
				}
				if strings.Contains(svc, tc.absent) && tc.absent == "builder-metrics" {
					t.Errorf("Service %s survived its component being turned off", svc)
				}
			}
		})
	}
}

// TestTheServiceMonitorScrapesBothControllersAndNotTheRegistry.
//
// The registry's Service carries the same two chart labels as the controllers', so a selector on
// those alone would have Prometheus scrape /metrics on a registry that serves the OCI API there —
// a scrape that fails quietly and that nobody investigates, because a ServiceMonitor that exists
// looks like monitoring that works.
func TestTheServiceMonitorScrapesBothControllersAndNotTheRegistry(t *testing.T) {
	out := render(t, "--set", "metrics.serviceMonitor.enabled=true")

	var found bool
	for _, doc := range strings.Split(out, "\n---") {
		var sm struct {
			Kind string `json:"kind"`
			Spec struct {
				Selector struct {
					MatchLabels      map[string]string `json:"matchLabels"`
					MatchExpressions []struct {
						Key      string   `json:"key"`
						Operator string   `json:"operator"`
						Values   []string `json:"values"`
					} `json:"matchExpressions"`
				} `json:"selector"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &sm); err != nil || sm.Kind != "ServiceMonitor" {
			continue
		}
		found = true

		if _, tooNarrow := sm.Spec.Selector.MatchLabels["app.kubernetes.io/component"]; tooNarrow {
			t.Error("a single component in matchLabels leaves the other controller unscraped")
		}
		if len(sm.Spec.Selector.MatchExpressions) == 0 {
			t.Fatal("without an expression this selects the registry too, whose :5000 is not metrics")
		}
		for _, e := range sm.Spec.Selector.MatchExpressions {
			if e.Key != "app.kubernetes.io/component" {
				continue
			}
			got := map[string]bool{}
			for _, v := range e.Values {
				got[v] = true
			}
			if !got["composer"] || !got["builder"] {
				t.Errorf("both controllers must be scraped, got %v", e.Values)
			}
			if got["registry"] {
				t.Error("the registry serves the OCI API on that port, not metrics")
			}
		}
	}
	if !found {
		t.Fatal("no ServiceMonitor rendered with metrics.serviceMonitor.enabled=true")
	}
}
