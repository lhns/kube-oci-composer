package controller

import (
	"strings"
	"testing"

	yamlv3 "go.yaml.in/yaml/v3"
)

// No rendered document may repeat a key. Kubernetes' decoders keep the last one, so a second
// block silently replaces the first: the composer's S3 credentials were dropped this way by a
// second `env:` key whenever operator.s3.existingSecret was set. yaml.v3 refuses duplicates.
func TestNoRenderedDocumentRepeatsAKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "defaults"},
		{name: "S3 credentials", args: []string{
			"--set", "operator.s3.existingSecret=s3creds",
			"--set", "operator.s3.bucket=b",
			"--set", "operator.s3.endpoint=https://s3.example",
		}},
		{name: "registry TLS", args: []string{
			"--set", "registry.tls.enabled=true", "--set", "registry.tls.mode=selfSigned",
		}},
		{name: "no ImageBuild", args: []string{"--set", "imageBuild.enabled=false"}},
		{name: "external registry", args: []string{
			"--set", "registry.enabled=false", "--set", "registry.publish.mode=external",
			"--set", "defaultRegistry.host=registry.example",
			"--set", "defaultRegistry.existingPushSecret=push",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, doc := range strings.Split(render(t, tc.args...), "\n---") {
				var v any
				if err := yamlv3.Unmarshal([]byte(doc), &v); err != nil {
					t.Errorf("document %d does not parse strictly: %v\n%s", i, err, doc)
				}
			}
		})
	}
}
