package controller

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestEveryMountedSecretAndConfigMapIsRendered is a structural guard, not a test of one bug.
//
// The bug it was written for: the registry Deployment mounted `<fullname>-registry-htpasswd`
// whenever `registry.auth.enabled`, while the template producing that Secret was gated on
// something else entirely (`not defaultRegistry.existingPushSecret`). One combination of values
// therefore rendered a pod referencing a Secret the same render did not create, and the pod wedged
// on it. Nothing in the chart tests looked at the relationship between the two.
//
// The class is what matters: a mount and the object it mounts are written in different files, under
// different conditions, by people making different changes. This asserts the invariant those
// conditions exist to preserve, across the toggle matrix, so the next divergence fails here rather
// than in someone's cluster.
//
// Scoped to names the CHART generates. A Secret the operator supplies -- `existingHtpasswdSecret`,
// `existingPushSecret`, a TLS Secret from cert-manager -- is deliberately absent from the render,
// and demanding it would make this guard fire on correct configurations, which is how guards get
// deleted.
func TestEveryMountedSecretAndConfigMapIsRendered(t *testing.T) {
	matrix := []struct {
		name string
		args []string
	}{
		{"defaults", nil},
		{"auth disabled", []string{"--set", "registry.auth.enabled=false"}},
		{"a pinned password", []string{"--set", "registry.auth.password=hunter2"}},
		{
			// The combination that produced the bug. It is refused outright now, so it is
			// exercised here in each of its resolved forms instead.
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

	// Names the operator supplies rather than the chart. Kept as values so the test says out loud
	// which references it is deliberately not checking.
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

			// Vacuity is asserted on the default install only. Some combinations legitimately
			// mount nothing -- with no bundled registry and no layer-cache PVC there is no
			// volume in the release at all -- and failing those would be asserting the wrong
			// thing. But if the DEFAULT render ever stops having mounts, this guard has quietly
			// stopped guarding, which is the failure mode worth catching.
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
