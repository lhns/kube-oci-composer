package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
)

// An unpack mode is spelled out in four hand-maintained places: the kubebuilder enum marker, the
// API's Unpack constants, the internal UnpackMode mirror, and the switch in collectEntries. These
// tests keep them in agreement, reading the generated CRD from disk because the marker is what a
// cluster actually enforces.

// allUnpackModes must list every API unpack mode. Deliberately manual: Go cannot enumerate a
// string type's constants, and a derived list would agree with the code for free.
var allUnpackModes = []ociv1alpha1.Unpack{
	ociv1alpha1.UnpackNone,
	ociv1alpha1.UnpackTar,
	ociv1alpha1.UnpackTarGz,
	ociv1alpha1.UnpackTarXz,
	ociv1alpha1.UnpackTarZstd,
	ociv1alpha1.UnpackTarBz2,
	ociv1alpha1.UnpackGz,
	ociv1alpha1.UnpackZip,
	ociv1alpha1.UnpackDeb,
}

// crdUnpackEnum digs the unpack enum out of the generated CRD.
func crdUnpackEnum(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "config", "crd", "bases", "oci.lhns.de_imagecompositions.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the generated CRD: %v", err)
	}

	// Walked untyped: the typed route needs apiextensions-apiserver, only an indirect dependency.
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the generated CRD: %v", err)
	}

	node := dig(t, doc, "spec", "versions")
	versions, ok := node.([]any)
	if !ok || len(versions) == 0 {
		t.Fatal("the generated CRD has no versions")
	}
	unpack := dig(t, versions[0], "schema", "openAPIV3Schema", "properties", "spec",
		"properties", "layers", "items", "properties", "fetch", "properties", "unpack", "enum")

	values, ok := unpack.([]any)
	if !ok || len(values) == 0 {
		t.Fatal("no unpack enum in the generated CRD; has the schema shape changed?")
	}
	enum := make([]string, 0, len(values))
	for _, v := range values {
		enum = append(enum, fmt.Sprint(v))
	}
	return enum
}

// dig walks nested maps, failing with the path it got stuck on rather than a nil panic.
func dig(t *testing.T, node any, path ...string) any {
	t.Helper()
	for i, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("CRD shape changed: %v is not a map", path[:i])
		}
		if node, ok = m[key]; !ok {
			t.Fatalf("CRD shape changed: no %v", path[:i+1])
		}
	}
	return node
}

// TestUnpackModesAreInTheCRDEnum — a mode missing from the CRD is unusable. Catches a forgotten
// marker, or one edited without regenerating.
func TestUnpackModesAreInTheCRDEnum(t *testing.T) {
	enum := crdUnpackEnum(t)
	listed := make(map[string]bool, len(enum))
	for _, v := range enum {
		listed[v] = true
	}

	for _, mode := range allUnpackModes {
		if !listed[string(mode)] {
			t.Errorf("unpack mode %q is implemented but missing from the CRD enum %v; "+
				"add it to the +kubebuilder:validation:Enum marker and regenerate", mode, enum)
		}
	}
}

// TestCRDEnumModesAreImplemented — the other direction: a mode the CRD admits but the code lacks
// is accepted and then fails during the build.
func TestCRDEnumModesAreImplemented(t *testing.T) {
	known := make(map[string]bool, len(allUnpackModes))
	for _, mode := range allUnpackModes {
		known[string(mode)] = true
	}

	for _, v := range crdUnpackEnum(t) {
		if !known[v] {
			t.Errorf("the CRD admits unpack mode %q, which is not in the implemented set; "+
				"either implement it or drop it from the enum", v)
		}
	}
}

// TestUnpackModesMirrorTheInternalConstants — internal/oci keeps its own copy of the enum (so it
// need not import the API types); resolve.go converts by cast, so the strings must match.
func TestUnpackModesMirrorTheInternalConstants(t *testing.T) {
	mirrors := map[ociv1alpha1.Unpack]oci.UnpackMode{
		ociv1alpha1.UnpackNone:    oci.UnpackNone,
		ociv1alpha1.UnpackTar:     oci.UnpackTar,
		ociv1alpha1.UnpackTarGz:   oci.UnpackTarGz,
		ociv1alpha1.UnpackTarXz:   oci.UnpackTarXz,
		ociv1alpha1.UnpackTarZstd: oci.UnpackTarZstd,
		ociv1alpha1.UnpackTarBz2:  oci.UnpackTarBz2,
		ociv1alpha1.UnpackGz:      oci.UnpackGz,
		ociv1alpha1.UnpackZip:     oci.UnpackZip,
		ociv1alpha1.UnpackDeb:     oci.UnpackDeb,
	}

	if len(mirrors) != len(allUnpackModes) {
		t.Errorf("mirrors has %d entries, allUnpackModes has %d: one of the two lists was not "+
			"updated", len(mirrors), len(allUnpackModes))
	}

	for _, mode := range allUnpackModes {
		mirror, ok := mirrors[mode]
		if !ok {
			t.Errorf("unpack mode %q has no internal counterpart", mode)
			continue
		}
		if string(mirror) != string(mode) {
			t.Errorf("unpack mode %q is mirrored internally as %q; the strings must match, "+
				"because resolve.go converts one to the other by cast", mode, mirror)
		}
	}
}
