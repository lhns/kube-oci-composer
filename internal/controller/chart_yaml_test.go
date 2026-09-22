package controller

import (
	"strconv"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// strictRenderCases are the value combinations the YAML guard renders; each enables templates the
// defaults leave out (the reader Deployment renders nothing at readReplicas=0).
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

// TestEveryRenderedDocumentHasUniqueKeys: helm accepts duplicate mapping keys (last wins), but
// Flux's strict post-renderer rejects them and the whole release upgrade fails.
func TestEveryRenderedDocumentHasUniqueKeys(t *testing.T) {
	for _, tc := range strictRenderCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, doc := range splitDocs(render(t, tc.args...)) {
				// yaml.v3 rejects a duplicate key; sigs.k8s.io/yaml silently keeps the last one.
				var into map[string]any
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

// describeDoc names a document for an error message without parsing it, since it may not parse.
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
