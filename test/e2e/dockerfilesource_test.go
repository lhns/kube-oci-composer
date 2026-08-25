//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// pinnedBase is the same image test/e2e/manifests/dockerfile uses, so a build here pulls something
// the cluster has already seen. Pinned because the controller refuses a floating FROM.
const pinnedBase = "busybox:1.37@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0"

// A Dockerfile that does not live in the build context, against a real cluster.

// TestAnInlineDockerfileBuilds — the motivating case: an upstream project that ships no Dockerfile,
// built without forking it to add one.
func TestAnInlineDockerfileBuilds(t *testing.T) {
	name := "e2e-inline"
	applyStdin(t, fmt.Sprintf(`
apiVersion: oci.lhns.de/v1alpha1
kind: ImageBuild
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1h
  context:
    sourceRef:
      kind: GitRepository
      name: e2e-src
  dockerfile:
    inline: |
      FROM %s
      COPY Dockerfile /recipe
  platforms: [linux/amd64]
  timeout: 10m
  push:
    repository: %s/e2e/%s
    tags: [v1]
`, name, buildNamespace, pinnedBase, buildRegistry, name))

	buildEventually(t, "the inline build to publish", func() error {
		st := buildStatus(t, name)
		if st.Artifact == nil {
			return fmt.Errorf("no artifact yet: %+v", st.Conditions)
		}
		return nil
	})
}

// TestAContextlessInlineBuildNeedsNoSource — a build that reads no files used to need a Flux
// source pointed at an empty directory, purely to satisfy a required field.
func TestAContextlessInlineBuildNeedsNoSource(t *testing.T) {
	name := "e2e-nocontext"
	applyStdin(t, fmt.Sprintf(`
apiVersion: oci.lhns.de/v1alpha1
kind: ImageBuild
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1h
  dockerfile:
    inline: |
      FROM %s
      RUN echo built-with-no-context > /marker
  platforms: [linux/amd64]
  timeout: 10m
  push:
    repository: %s/e2e/%s
    tags: [v1]
`, name, buildNamespace, pinnedBase, buildRegistry, name))

	buildEventually(t, "the context-less build to publish", func() error {
		st := buildStatus(t, name)
		if st.Artifact == nil {
			return fmt.Errorf("no artifact yet: %+v", st.Conditions)
		}
		return nil
	})
}

// TestADockerfileFromAConfigMapBuildsAndRebuildsOnEdit — two claims: the content is hashed, so an
// edit rebuilds, and the ConfigMap is watched, so it happens before the next interval an hour on.
func TestADockerfileFromAConfigMapBuildsAndRebuildsOnEdit(t *testing.T) {
	name := "e2e-configmap"
	apply := func(marker string) {
		applyStdin(t, fmt.Sprintf(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: %s-recipe
  namespace: %s
data:
  Dockerfile: |
    FROM %s
    RUN echo %s > /marker
`, name, buildNamespace, pinnedBase, marker))
	}
	apply("first")

	applyStdin(t, fmt.Sprintf(`
apiVersion: oci.lhns.de/v1alpha1
kind: ImageBuild
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1h
  dockerfile:
    configMapRef: {name: %s-recipe, key: Dockerfile}
  platforms: [linux/amd64]
  timeout: 10m
  push:
    repository: %s/e2e/%s
    tags: [v1]
    onConflict: Overwrite
`, name, buildNamespace, name, buildRegistry, name))

	var first string
	buildEventually(t, "the ConfigMap build to publish", func() error {
		st := buildStatus(t, name)
		if st.Artifact == nil {
			return fmt.Errorf("no artifact yet: %+v", st.Conditions)
		}
		first = st.InputHash
		return nil
	})

	// Only the ConfigMap changes. If the hash covered the reference rather than the content, or
	// nothing watched the ConfigMap, this would never finish.
	apply("second")
	buildEventually(t, "the edit to move the input hash", func() error {
		st := buildStatus(t, name)
		if st.InputHash == first {
			return fmt.Errorf("the input hash is unchanged after editing the ConfigMap: %s", first)
		}
		return nil
	})
}

// TestTheFromGuardRunsWhateverTheDockerfileCameFrom is the most important test in this file: a
// source that skips the only content guard this controller has is a security regression.
//
// The guard runs in the controller for a path, an inline or a ConfigMap, and in the build pod's
// fetcher for an image context. A table, so a fourth source with no case here is visibly missing.
func TestTheFromGuardRunsWhateverTheDockerfileCameFrom(t *testing.T) {
	unpinned := "FROM golang:1.26\n"

	for _, tc := range []struct{ name, spec string }{
		{
			// The original: the recipe is in the Flux source.
			name: "path in the context",
			spec: `
  context:
    sourceRef: {kind: GitRepository, name: e2e-src}
  dockerfile: {path: Dockerfile.unpinned}`,
		},
		{
			// Terminal here, unlike the others: the Dockerfile IS this spec.
			name: "inline",
			spec: `
  dockerfile:
    inline: |
      ` + strings.TrimSpace(unpinned),
		},
		{
			name: "from a ConfigMap",
			spec: `
  dockerfile:
    configMapRef: {name: e2e-unpinned-recipe, key: Dockerfile}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Lowercased: an object name is an RFC 1123 subdomain, and "from a ConfigMap"
			// carries capitals the API server refuses.
			name := "e2e-guard-" + strings.ToLower(strings.ReplaceAll(tc.name, " ", "-"))
			if strings.Contains(tc.name, "ConfigMap") {
				applyStdin(t, fmt.Sprintf(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: e2e-unpinned-recipe
  namespace: %s
data:
  Dockerfile: |
    %s
`, buildNamespace, strings.TrimSpace(unpinned)))
			}

			applyStdin(t, fmt.Sprintf(`
apiVersion: oci.lhns.de/v1alpha1
kind: ImageBuild
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1h%s
  platforms: [linux/amd64]
  timeout: 10m
  push:
    repository: %s/e2e/%s
    tags: [v1]
`, name, buildNamespace, tc.spec, buildRegistry, name))

			buildEventually(t, "the unpinned FROM to be refused", func() error {
				st := buildStatus(t, name)
				if st.Artifact != nil {
					return fmt.Errorf("an unpinned FROM produced an artifact: %+v", st.Artifact)
				}
				if c := readyCondition(st); c != nil && c.Status == "False" &&
					strings.Contains(c.Message, "pinned by digest") {
					return nil
				}
				return fmt.Errorf("not refused yet: %+v", st.Conditions)
			})

			// Nothing may have been published, whichever way it was refused.
			jobs := mustKubectl(t, "-n", buildNamespace, "get", "jobs",
				"-o", "jsonpath={.items[*].metadata.name}")
			if strings.Contains(tc.name, "context") && strings.Contains(jobs, name) {
				t.Errorf("a Job was created for an unpinned FROM the controller could read: %s", jobs)
			}
		})
	}
}
