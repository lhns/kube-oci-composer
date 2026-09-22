package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Builder-component chart tests. They live here to reuse this package's chart helpers; the builder
// is a component of the one chart (ADR F), so they inspect its objects in the full render.

const builderChartDir = "../../charts/kube-oci-composer"

// builderRenderArgs are the values the chart refuses to render without: the two pinned images and
// the publish mode.
var builderRenderArgs = append([]string{
	"--set", "imageBuild.buildkitImage=moby/buildkit:v1@sha256:" + strings.Repeat("a", 64),
	"--set", "imageBuild.dockerfileFrontend=docker/dockerfile:1@sha256:" + strings.Repeat("b", 64),
}, installable...)

func renderBuilder(t *testing.T, args ...string) string {
	t.Helper()
	out, err := helmTemplate(t, "oci-builder", append(append([]string{}, builderRenderArgs...), args...)...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

// TestBuilderChartRBACMatchesTheGeneratedRole: the builder's role can create Jobs, i.e. run
// arbitrary containers, so its hand-written chart RBAC must match the kubebuilder markers exactly.
func TestBuilderChartRBACMatchesTheGeneratedRole(t *testing.T) {
	generated, err := readClusterRole(
		filepath.Join("..", "..", "config", "rbac-builder", "role.yaml"))
	if err != nil {
		t.Fatalf("reading generated role: %v", err)
	}

	chart := clusterRoleFromRender(t, renderBuilder(t), "test-release-kube-oci-composer-builder")

	// Leader election lives in a namespaced Role in the chart.
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

// TestBuilderChartNeverGrantsSecretListOrWatch: the builder only gets Secrets it references;
// list/watch would expose every Secret to a controller that runs user code.
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

// TestBuilderChartFlagsMatchTheBinary: an unknown flag crash-loops the container.
func TestBuilderChartFlagsMatchTheBinary(t *testing.T) {
	// Optional flags set, or the `with`-wrapped ones are never checked.
	out := renderBuilder(t, "--set", "imageBuild.insecureRegistry=registry.internal:5000")
	known := knownFlags(t, "../../cmd/oci-builder")

	// Only the builder's container: the composer's flags belong to a different binary.
	assertFlagsKnown(t, containerArgs(t, out, "test-release-kube-oci-composer-builder"), known)
}

// TestBuilderChartRefusesUnpinnedBuilderImages: the pin is what makes the input hash honest.
func TestBuilderChartRefusesUnpinnedBuilderImages(t *testing.T) {
	for _, field := range []string{"buildkitImage", "dockerfileFrontend"} {
		t.Run(field, func(t *testing.T) {
			args := append(append([]string{}, builderRenderArgs...),
				"--set", "imageBuild."+field+"=some/image:latest")

			out, err := helmTemplate(t, "oci-builder", args...)
			if err == nil {
				t.Fatalf("an unpinned %s rendered successfully:\n%s", field, out)
			}
			if !strings.Contains(out, "must be pinned by digest") {
				t.Errorf("the failure does not explain the rule:\n%s", out)
			}
		})
	}
}

// TestBuilderChartShipsRealDigests: the guard above accepts a placeholder "@sha256:" digest, which
// would make every build fail to pull.
func TestBuilderChartShipsRealDigests(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(builderChartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("reading values.yaml: %v", err)
	}
	if strings.Contains(string(raw), "sha256:"+strings.Repeat("0", 64)) {
		t.Error("values.yaml pins a placeholder all-zero digest; builds would fail to pull")
	}
}

// containerArgs returns the args of the named Deployment's first container, as rendered lines.
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
			// EXACT match: "name: x-composer" is a prefix of "name: x-composer-builder".
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

// TestBothChartsGrantConfigMapWritesButNeverInBulk: the ConfigMap verbs are cluster-wide (the
// export boundary lives in the controller, ADR 0056), but never deletecollection or a wildcard.
// delete must be present, or the finalizer on a cross-namespace export fails Forbidden and the
// object never finishes deleting.
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

// TestNoPerNamespaceExportRoleIsRendered: with the ClusterRole carrying the verbs, a per-namespace
// Role would be inert RBAC that looks like a boundary (ADR 0056).
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

// TestBothControllersGetTheExportFlags: a setting reaching only one kind is a silent
// half-configuration.
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

// TestTheWatchLabelDoesNotDependOnTheAllowList: an export into the object's OWN namespace needs no
// allow-list, and must still get the watch label.
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
