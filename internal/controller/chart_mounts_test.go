package controller

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestEveryMountedSecretAndConfigMapIsRendered: across the toggle matrix, every Secret/ConfigMap a
// workload mounts is created by the same render, or the pod wedges. Mounts and the objects they
// reference are gated by conditions in different templates, so they can diverge. Operator-supplied
// names are deliberately excluded.
func TestEveryMountedSecretAndConfigMapIsRendered(t *testing.T) {
	matrix := []struct {
		name string
		args []string
	}{
		{"defaults", nil},
		{"auth disabled", []string{"--set", "registry.auth.enabled=false"}},
		{"a pinned password", []string{"--set", "registry.auth.password=hunter2"}},
		{
			// A divergent combination; refused as such now, so each resolved form is exercised.
			"own push credential, own htpasswd",
			[]string{
				"--set", "defaultRegistry.existingPushSecret=mine",
				"--set", "registry.auth.existingHtpasswdSecret=my-htpasswd",
			},
		},
		{
			"own push credential, chart-managed password",
			[]string{
				"--set", "defaultRegistry.existingPushSecret=mine",
				"--set", "registry.auth.password=hunter2",
			},
		},
		{
			"own push credential, registry unauthenticated",
			[]string{
				"--set", "defaultRegistry.existingPushSecret=mine",
				"--set", "registry.auth.enabled=false",
			},
		},
		{"no bundled registry", []string{
			"--set", "registry.enabled=false",
			"--set", "defaultRegistry.host=ghcr.io/example",
			"--set", "defaultRegistry.existingPushSecret=mine",
		}},
		{"no persistence", []string{"--set", "registry.persistence.enabled=false"}},
		{"builds disabled", []string{"--set", "imageBuild.enabled=false"}},
		{"compositions disabled", []string{"--set", "imageComposition.enabled=false"}},
	}

	// Names the operator supplies rather than the chart.
	supplied := map[string]bool{"mine": true, "my-htpasswd": true}

	for _, tc := range matrix {
		t.Run(tc.name, func(t *testing.T) {
			out := render(t, tc.args...)

			created := map[string]bool{}
			type mount struct{ workload, kind, name string }
			var mounts []mount

			for _, doc := range strings.Split(out, "\n---") {
				var obj struct {
					Kind     string `json:"kind"`
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
					Spec struct {
						Template struct {
							Spec struct {
								Volumes []struct {
									Secret *struct {
										SecretName string `json:"secretName"`
									} `json:"secret"`
									ConfigMap *struct {
										Name string `json:"name"`
									} `json:"configMap"`
								} `json:"volumes"`
							} `json:"spec"`
						} `json:"template"`
					} `json:"spec"`
				}
				if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
					continue
				}
				switch obj.Kind {
				case "Secret", "ConfigMap":
					created[obj.Kind+"/"+obj.Metadata.Name] = true
				case "Deployment", "StatefulSet", "DaemonSet", "Job":
					for _, v := range obj.Spec.Template.Spec.Volumes {
						if v.Secret != nil && v.Secret.SecretName != "" {
							mounts = append(mounts, mount{obj.Metadata.Name, "Secret", v.Secret.SecretName})
						}
						if v.ConfigMap != nil && v.ConfigMap.Name != "" {
							mounts = append(mounts, mount{obj.Metadata.Name, "ConfigMap", v.ConfigMap.Name})
						}
					}
				}
			}

			// Vacuity is checked on the defaults only: some combinations legitimately mount
			// nothing.
			if tc.name == "defaults" && len(mounts) == 0 {
				t.Fatal("the default render mounts nothing; this guard would pass vacuously")
			}
			for _, m := range mounts {
				if supplied[m.name] {
					continue
				}
				if !created[m.kind+"/"+m.name] {
					t.Errorf("%s mounts %s %q, which this render does not create — "+
						"the pod would wedge waiting for it", m.workload, m.kind, m.name)
				}
			}
		})
	}
}
