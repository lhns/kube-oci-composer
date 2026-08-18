package controller

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The builder chart, held to the same standard as the composer's.
//
// These live here rather than in internal/buildcontroller because the helpers they need —
// render-and-parse, ruleSet, knownFlags — already exist in this package, and standing up a second
// copy next to the builder would be the drift this file exists to prevent.
//
// Both guards mirror an existing composer test. Without the RBAC one, config/rbac-builder/role.yaml
// is output nothing reads, and the chart's hand-written rules can diverge from the kubebuilder
// markers with nothing noticing.

const builderChartDir = "../../charts/kube-oci-builder"

// pinned values the builder chart refuses to render without.
var builderRenderArgs = []string{
	"--set", "builder.buildkitImage=moby/buildkit:v1@sha256:" + strings.Repeat("a", 64),
	"--set", "builder.dockerfileFrontend=docker/dockerfile:1@sha256:" + strings.Repeat("b", 64),
}

func renderBuilder(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; skipping chart render")
	}

	base := []string{"template", "test-release", builderChartDir, "--namespace", "oci-builder"}
	full := append(append(base, builderRenderArgs...), args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// TestBuilderChartRBACMatchesTheGeneratedRole — the drift guard that matters most, and the reason
// the generated role is worth generating.
//
// This role is the difference between the builder and the composer: it can create Jobs, which is
// the ability to run arbitrary containers. Too few verbs and the controller fails at runtime
// looking like a bug; too many and it holds permissions nobody reviewed.
func TestBuilderChartRBACMatchesTheGeneratedRole(t *testing.T) {
	generated, err := readClusterRole(
		filepath.Join("..", "..", "config", "rbac-builder", "role.yaml"))
	if err != nil {
		t.Fatalf("reading generated role: %v", err)
	}

	chart := clusterRoleFromRender(t, renderBuilder(t), "test-release-kube-oci-builder")

	// Leader election lives in a namespaced Role in the chart, as it does for the composer.
	want := ruleSet(rulesExcluding(generated, "coordination.k8s.io"))
	got := ruleSet(chart.Rules)

	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("builder chart ClusterRole is MISSING a rule the controller needs: %s", k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("builder chart ClusterRole grants a rule the controller does not use: %s", k)
		}
	}
}

// TestBuilderChartNeverGrantsSecretListOrWatch — the builder reads a Secret's resourceVersion so a
// rotation rebuilds, and projects its value into the build pod. Neither needs list or watch, and
// granting them would put every Secret in the cluster behind a controller that runs user code.
func TestBuilderChartNeverGrantsSecretListOrWatch(t *testing.T) {
	chart := clusterRoleFromRender(t, renderBuilder(t), "test-release-kube-oci-builder")

	for _, rule := range chart.Rules {
		if !containsString(rule.APIGroups, "") || !containsString(rule.Resources, "secrets") {
			continue
		}
		for _, verb := range rule.Verbs {
			if verb == "list" || verb == "watch" || verb == "*" {
				t.Fatalf("builder chart grants %q on secrets; it must be get only", verb)
			}
		}
	}
}

// TestBuilderChartFlagsMatchTheBinary — every flag the chart renders must exist, or the container
// crash-loops on an unknown flag with nothing else to say why.
func TestBuilderChartFlagsMatchTheBinary(t *testing.T) {
	out := renderBuilder(t)
	known := knownFlags(t, "../../cmd/oci-builder")

	var seen int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- --") {
			continue
		}
		flag := strings.TrimPrefix(line, "- --")
		if i := strings.Index(flag, "="); i >= 0 {
			flag = flag[:i]
		}
		if _, ok := known[flag]; !ok {
			t.Errorf("builder chart renders --%s, which the binary does not define", flag)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no flags found in the rendered output; the assertion proves nothing")
	}
}

// TestBuilderChartRefusesUnpinnedBuilderImages — the pin is what makes the input hash honest, so
// the failure belongs at template time where the message can say so.
func TestBuilderChartRefusesUnpinnedBuilderImages(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; skipping chart render")
	}

	for _, field := range []string{"buildkitImage", "dockerfileFrontend"} {
		t.Run(field, func(t *testing.T) {
			args := append([]string{
				"template", "test-release", builderChartDir, "--namespace", "oci-builder",
			}, builderRenderArgs...)
			args = append(args, "--set", "builder."+field+"=some/image:latest")

			out, err := exec.Command("helm", args...).CombinedOutput()
			if err == nil {
				t.Fatalf("an unpinned %s rendered successfully:\n%s", field, out)
			}
			if !strings.Contains(string(out), "must be pinned by digest") {
				t.Errorf("the failure does not explain the rule:\n%s", out)
			}
		})
	}
}

// TestBuilderChartShipsRealDigests — the guard above checks for "@sha256:", which a placeholder
// satisfies. An all-zero digest shipped once and made every build fail to pull.
func TestBuilderChartShipsRealDigests(t *testing.T) {
	raw, err := readFile(filepath.Join(builderChartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("reading values.yaml: %v", err)
	}
	if strings.Contains(string(raw), "sha256:"+strings.Repeat("0", 64)) {
		t.Error("values.yaml pins a placeholder all-zero digest; builds would fail to pull")
	}
}
