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
// The first four mirror a composer test; TestBuilderChartShipsRealDigests does not. Without the
// RBAC one, config/rbac-builder/role.yaml is output nothing reads, and the chart's hand-written
// rules can diverge from the kubebuilder markers with nothing noticing.

// One chart now (ADR F). The builder is a component of it, so these render the composer chart and
// look at the builder's objects inside the result -- which is also what makes the toggle testable:
// with imageBuild.enabled=false there must be no Job-creating role at all.
const builderChartDir = "../../charts/kube-oci-composer"

// Values the chart refuses to render without: the two pinned images, and how workloads reach the
// registry. None of these tests is about either question, so they supply an answer and get on with
// what they are actually asserting.
var builderRenderArgs = append([]string{
	"--set", "imageBuild.buildkitImage=moby/buildkit:v1@sha256:" + strings.Repeat("a", 64),
	"--set", "imageBuild.dockerfileFrontend=docker/dockerfile:1@sha256:" + strings.Repeat("b", 64),
}, installable...)

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
	return helmOut(out)
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

	chart := clusterRoleFromRender(t, renderBuilder(t), "test-release-kube-oci-composer-builder")

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
	chart := clusterRoleFromRender(t, renderBuilder(t), "test-release-kube-oci-composer-builder")

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
	// Rendered WITH the optional flags set, or the guard silently skips them: insecureRegistry is
	// wrapped in a `with` block, so the newest flag on the chart would be the one flag a typo could
	// hide in.
	out := renderBuilder(t, "--set", "imageBuild.insecureRegistry=registry.internal:5000")
	known := knownFlags(t, "../../cmd/oci-builder")

	// Scoped to the BUILDER's container. One chart renders both controllers now, so scanning the
	// whole document would check the composer's flags against the builder's binary and report every
	// one of them as unknown -- a failure that says nothing about the thing under test.
	var seen int
	for _, line := range strings.Split(containerArgs(t, out, "test-release-kube-oci-composer-builder"), "\n") {
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
			args = append(args, "--set", "imageBuild."+field+"=some/image:latest")

			out, err := exec.Command("helm", args...).CombinedOutput()
			if err == nil {
				t.Fatalf("an unpinned %s rendered successfully:\n%s", field, out)
			}
			if !strings.Contains(helmOut(out), "must be pinned by digest") {
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

// containerArgs returns just the args of the named Deployment's first container, as rendered lines.
//
// The chart deploys three workloads into one namespace now, so "what does this chart render" stopped
// being a question with one answer.
func containerArgs(t *testing.T, rendered, deployment string) string {
	t.Helper()

	var out []string
	inDoc, inArgs := false, false
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "---") {
			inDoc, inArgs = false, false
			continue
		}
		if !inDoc {
			// EXACT match on the trimmed line. "name: x-composer" is a prefix of
			// "name: x-composer-builder", so a Contains here silently pulls in the other
			// controller's container and checks its flags against the wrong binary.
			if strings.TrimSpace(line) == "name: "+deployment {
				inDoc = true
			}
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(line), "args:") {
			inArgs = true
			continue
		}
		if inArgs {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "- ") {
				out = append(out, trimmed)
				continue
			}
			inArgs = false
		}
	}
	if len(out) == 0 {
		t.Fatalf("no args found for %s; the assertion would prove nothing", deployment)
	}
	return strings.Join(out, "\n")
}

// TestBothChartsGrantConfigMapWritesButNeverInBulk.
//
// ADR 0056 moved this boundary from RBAC into the controller: the verbs are cluster-wide because
// RBAC is granted before an object exists, so permitting an export into whatever namespace its
// object lives in means permitting it everywhere.
//
// What RBAC still says is that nothing here removes ConfigMaps in BULK. A create/update/delete
// mistake damages one object at a time and is guarded by the managed-by and owner labels;
// deletecollection or a wildcard is a different order of accident, and neither controller has any
// use for one.
//
// The delete verb is asserted present, not merely permitted: without it the finalizer on an export
// into another namespace fails Forbidden and the object never finishes deleting. That was the
// state of the chart when this was written.
func TestBothChartsGrantConfigMapWritesButNeverInBulk(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		role string
	}{
		{"builder", renderBuilder(t), "test-release-kube-oci-composer-builder"},
		{"composer", render(t), "test-release-kube-oci-composer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chart := clusterRoleFromRender(t, tc.out, tc.role)

			var seen bool
			verbs := map[string]bool{}
			for _, rule := range chart.Rules {
				if !containsString(rule.APIGroups, "") || !containsString(rule.Resources, "configmaps") {
					continue
				}
				seen = true
				for _, verb := range rule.Verbs {
					verbs[verb] = true
					if verb == "deletecollection" || verb == "*" {
						t.Errorf("grants %q on configmaps; nothing removes them in bulk", verb)
					}
				}
			}
			if !seen {
				t.Fatal("no configmaps rule at all; a ConfigMap layer could not be read")
			}
			for _, want := range []string{"get", "list", "watch", "create", "update", "delete"} {
				if !verbs[want] {
					t.Errorf("missing %q on configmaps: push.writeRefTo needs the write verbs, and "+
						"without delete the finalizer fails Forbidden and blocks deletion", want)
				}
			}
		})
	}
}

// TestNoPerNamespaceExportRoleIsRendered.
//
// There used to be a Role and RoleBinding per allow-listed namespace. Once the ClusterRole carried
// the verbs it granted nothing further, and RBAC that looks like a boundary while being inert is
// worse than none -- somebody reads it and believes it. ADR 0056.
func TestNoPerNamespaceExportRoleIsRendered(t *testing.T) {
	out := renderBuilder(t, "--set", `refExport.namespaces={flux-system,team-a}`)
	for _, d := range docs(t, out) {
		k, _ := d["kind"].(string)
		if k != "Role" && k != "RoleBinding" {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		if name, _ := meta["name"].(string); strings.Contains(name, "refexport") {
			t.Errorf("a per-namespace export %s is still rendered: %s", k, name)
		}
	}
}

// TestBothControllersGetTheExportFlags -- the feature is identical on both kinds, so a setting
// that reached only one would be a silent half-configuration.
func TestBothControllersGetTheExportFlags(t *testing.T) {
	args := []string{
		"--set", `refExport.namespaces={flux-system}`,
		"--set", `refExport.labels=reconcile.fluxcd.io/watch=Enabled`,
		"--set", `refExport.allowedLabels={team}`,
		"--set", `refExport.allowedAnnotations={example.com/*}`,
	}
	for _, tc := range []struct {
		name       string
		got        string
		deployment string
	}{
		{"builder", renderBuilder(t, args...), "test-release-kube-oci-composer-builder"},
		{"composer", render(t, args...), "test-release-kube-oci-composer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := containerArgs(t, tc.got, tc.deployment)
			for _, want := range []string{
				"--ref-export-namespaces=flux-system",
				"--ref-export-labels=reconcile.fluxcd.io/watch=Enabled",
				"--ref-export-allowed-labels=team",
				"--ref-export-allowed-annotations=example.com/*",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q; got:\n%s", want, got)
				}
			}
		})
	}
}

// TestTheWatchLabelDoesNotDependOnTheAllowList.
//
// An export into the object's OWN namespace needs no allow-list, so a chart that rendered
// --ref-export-labels only alongside --ref-export-namespaces would leave those exports unwatched.
// It did, while the allow-list was the only way to export at all.
func TestTheWatchLabelDoesNotDependOnTheAllowList(t *testing.T) {
	out := renderBuilder(t, "--set", `refExport.labels=reconcile.fluxcd.io/watch=Enabled`)
	got := containerArgs(t, out, "test-release-kube-oci-composer-builder")
	if !strings.Contains(got, "--ref-export-labels=reconcile.fluxcd.io/watch=Enabled") {
		t.Errorf("the watch label was dropped without an allow-list; got:\n%s", got)
	}
	if strings.Contains(got, "--ref-export-namespaces") {
		t.Errorf("an allow-list was rendered from nothing; got:\n%s", got)
	}
}
