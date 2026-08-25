package controller

import (
	"strconv"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// strictRenderCases are the value combinations the YAML guard renders.
//
// A matrix rather than the defaults alone, and readReplicas > 0 is the row that earns its keep:
// registry-reader.yaml renders nothing at readReplicas: 0, so the reader Deployment -- which
// carried the same duplicate-key bug as the writer -- is invisible to a default-values check. The
// bug report against this chart missed exactly that, for exactly that reason.
var strictRenderCases = []struct {
	name string
	args []string
}{
	{"defaults", nil},
	{"read replicas", readReplicaArgs(3)},
	{"tls", []string{"--set", "registry.tls.enabled=true"}},
	{"read replicas and tls", append(readReplicaArgs(2), "--set", "registry.tls.enabled=true")},
	{"registry disabled", []string{
		"--set", "registry.enabled=false",
		"--set", "defaultRegistry.host=ghcr.io",
	}},
}

// readReplicaArgs is the smallest set of values that makes read replicas render: the chart refuses
// them without a store every pod can see and a shared metadata database.
func readReplicaArgs(n int) []string {
	return []string{
		"--set", "registry.readReplicas=" + strconv.Itoa(n),
		"--set", "registry.persistence.accessMode=ReadWriteMany",
		"--set", "registry.cache.driver=redis",
		"--set", "registry.cache.redis.url=redis://redis:6379",
	}
}

// TestEveryRenderedDocumentHasUniqueKeys is the guard for a class of break that every other check
// here is blind to.
//
// helm DOES parse what it renders -- a syntax error fails `helm template` outright, measured, not
// assumed. What it does not do is reject a DUPLICATE MAPPING KEY: its YAML-to-JSON conversion takes
// last-wins, so the chart renders, lints and tests clean while carrying YAML that a strict parser
// refuses. Flux's post-renderer is strict, so the break appears only on a real cluster.
//
// Because the CRDs live in templates/ and a Helm release is atomic, that rejection blocks the
// entire upgrade -- CRDs included -- so an API change cannot reach any cluster installing by chart.
//
// This reached users as a failed upgrade rather than a failed build. It should fail here instead.
func TestEveryRenderedDocumentHasUniqueKeys(t *testing.T) {
	for _, tc := range strictRenderCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, doc := range splitDocs(render(t, tc.args...)) {
				var into map[string]any
				// yaml.v3 rejects a duplicate mapping key; sigs.k8s.io/yaml, which the other chart
				// tests use, goes through JSON and silently keeps the last one.
				if err := yaml.Unmarshal([]byte(doc), &into); err != nil {
					t.Errorf("%s does not parse strictly: %v", describeDoc(doc), err)
				}
			}
		})
	}
}

// splitDocs cuts a helm render into documents.
func splitDocs(out string) []string {
	var docs []string
	for _, d := range strings.Split(out, "\n---\n") {
		if strings.TrimSpace(d) != "" {
			docs = append(docs, d)
		}
	}
	return docs
}

// describeDoc names a document for an error message, without parsing it -- the caller is reporting
// a document that would not parse.
func describeDoc(doc string) string {
	kind, name := "unknown kind", ""
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case strings.HasPrefix(line, "kind:"):
			kind = strings.TrimSpace(strings.TrimPrefix(line, "kind:"))
		case name == "" && strings.HasPrefix(line, "  name:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "  name:"))
		}
	}
	if name == "" {
		return kind
	}
	return kind + "/" + name
}
