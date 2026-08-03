// Command schemagen converts generated CRDs into standalone JSON schemas for kubeconform.
//
// This exists so a GitOps repository can validate ImageComposition manifests in CI, before
// anything reaches a cluster. kubeconform cannot read a CRD directly; it wants one schema file per
// kind, named by the convention below.
//
// Usage: schemagen <crd-dir> <out-dir>
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

type crd struct {
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind string `json:"kind"`
		} `json:"names"`
		Versions []struct {
			Name   string `json:"name"`
			Schema struct {
				OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: schemagen <crd-dir> <out-dir>")
		os.Exit(2)
	}
	crdDir, outDir := os.Args[1], os.Args[2]

	entries, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		fail(err)
	}
	if len(entries) == 0 {
		fail(fmt.Errorf("no CRDs found in %s; run 'make manifests' first", crdDir))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fail(err)
	}

	for _, path := range entries {
		raw, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		var c crd
		if err := yaml.Unmarshal(raw, &c); err != nil {
			fail(fmt.Errorf("%s: %w", path, err))
		}

		for _, v := range c.Spec.Versions {
			schema := v.Schema.OpenAPIV3Schema
			if schema == nil {
				continue
			}
			// kubeconform is strict by default, and a CRD schema legitimately omits fields the
			// API server fills in. Saying so explicitly beats every consumer passing -ignore-*.
			schema["$schema"] = "http://json-schema.org/draft-07/schema#"

			// <kind>-<group>-<version>.json, lowercased: the layout kubeconform's
			// -schema-location template expects.
			name := fmt.Sprintf("%s-%s-%s.json",
				strings.ToLower(c.Spec.Names.Kind), strings.ToLower(c.Spec.Group), v.Name)

			out, err := json.MarshalIndent(schema, "", "  ")
			if err != nil {
				fail(err)
			}
			out = append(out, '\n')
			if err := os.WriteFile(filepath.Join(outDir, name), out, 0o644); err != nil {
				fail(err)
			}
			fmt.Println("wrote", filepath.Join(outDir, name))
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "schemagen:", err)
	os.Exit(1)
}
