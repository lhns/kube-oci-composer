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
// API's Unpack constants, the internal UnpackMode constants that mirror them, and the switch in
// collectEntries. Nothing made them agree, and each way of disagreeing fails differently:
//
//   - a mode in the code but not the enum is unusable, and the only symptom is a rejection at
//     apply time that every unit test passes straight through
//   - a mode in the enum but not the switch is admitted and then fails during the build
//
// This is the guard that makes adding the next format safe. It reads the generated CRD from disk on
// purpose: the marker is the thing that can be forgotten, and asserting against the Go constants
// alone would prove nothing about what a cluster will accept.

// allUnpackModes is the list under test, and adding a mode to the API means adding it here. That is
// deliberately manual — Go has no way to enumerate a string type's constants, and a test that
// derived the list from the same place as the code under test would agree with itself for free.
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

	// Walked untyped rather than through a struct: the typed route needs either a 25-level nested
	// anonymous struct to reach one leaf, or apiextensions-apiserver, which is only an indirect
	// dependency today. dig keeps the path readable as the path it is.
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

// TestUnpackModesAreInTheCRDEnum — a mode the CRD does not list cannot be used at all, however
// complete its implementation is. Catches a forgotten kubebuilder marker, or a marker edited
// without running the generators.
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

// TestCRDEnumModesAreImplemented — the other direction. A mode the CRD admits but the code does not
// implement is accepted by the API server and then fails during the build, which is a far worse
// failure than a rejection because it only shows up once someone depends on it.
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

// TestUnpackModesMirrorTheInternalConstants — internal/oci keeps its own copy of the enum, so that
// the assembly package does not import the API types. A copy that drifts sends an unrecognised
// string into collectEntries' switch and lands on its default arm.
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
