package oci

import (
	"strings"
	"testing"
)

// TestProvenanceSurvivesTheObject covers threat-model gap R1.
//
// `status.history[].sources` already answers "what produced this artifact" -- but only while the
// object exists. Delete the ImageComposition and the answer goes with it, while the image it
// produced is still running somewhere. These annotations put the record in the artifact.
func TestProvenanceSurvivesTheObject(t *testing.T) {
	inputs := []LayerInput{{
		Name:   "bundle",
		Digest: "sha256:1111",
		Unpack: UnpackTarGz,
		Target: "/plugins",
		Path:   writeTarGz(t, map[string]string{"lib/a.jar": "aaa"}),
	}}

	img, err := AssembleAs(nil, inputs, Config{}, Platform{OS: "linux", Architecture: "amd64"}, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	mf, err := img.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if got := mf.Annotations[AnnotationSources]; got != "bundle=sha256:1111" {
		t.Errorf("sources annotation is %q, want the layer's name and digest", got)
	}
	if got := mf.Annotations[AnnotationAssemblyVersion]; got != "2" {
		t.Errorf("assembly-version annotation is %q, want the current AssemblyVersion", got)
	}
	// Absent, not empty: a scratch artifact has no base, and an empty string would read as one
	// whose digest could not be determined.
	if _, ok := mf.Annotations[AnnotationBase]; ok {
		t.Errorf("a scratch artifact must not claim a base: %v", mf.Annotations)
	}
}

// TestProvenanceRecordsTheRevisionRatherThanTheTarball is the detail that makes the annotation
// useful rather than merely present.
//
// source-controller re-packs its artifacts on restart, so a Flux tarball's digest changes while
// the revision it describes does not. The tarball digest answers "which bytes did we fetch"; the
// revision answers "what produced this", which is the question R1 asks. InputHash already prefers
// Identity for the same reason.
func TestProvenanceRecordsTheRevisionRatherThanTheTarball(t *testing.T) {
	inputs := []LayerInput{{
		Name:     "config",
		Digest:   "sha256:2222", // the tarball, which moves
		Identity: "main@sha1:abcd",
		Unpack:   UnpackTarGz,
		Target:   "/config",
		Path:     writeTarGz(t, map[string]string{"app.conf": "x"}),
	}}

	img, err := AssembleAs(nil, inputs, Config{}, Platform{OS: "linux", Architecture: "amd64"}, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	mf, err := img.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	got := mf.Annotations[AnnotationSources]
	if !strings.Contains(got, "main@sha1:abcd") {
		t.Errorf("the revision must be what is recorded, got %q", got)
	}
	if strings.Contains(got, "sha256:2222") {
		t.Errorf("the tarball digest moves and does not identify the source; got %q", got)
	}
}

// TestProvenanceKeepsSpecOrder — layer order is semantic, since a later layer overwrites an
// earlier one. Sorting the annotation would discard that.
func TestProvenanceKeepsSpecOrder(t *testing.T) {
	ann := provenanceAnnotations(nil, []LayerInput{
		{Name: "zzz", Digest: "sha256:1"},
		{Name: "aaa", Digest: "sha256:2"},
	})
	if got := ann[AnnotationSources]; got != "zzz=sha256:1 aaa=sha256:2" {
		t.Errorf("sources annotation is %q, want spec order", got)
	}
}

// TestProvenanceIsDeterministic is the constraint that outranks the feature.
//
// `output digest = f(spec)` is the core invariant (ADR 0016). An annotation carrying a build time,
// a hostname or anything else observed at runtime would end it -- which is why
// org.opencontainers.image.created is deliberately NOT written here.
func TestProvenanceIsDeterministic(t *testing.T) {
	inputs := []LayerInput{{
		Name:   "bundle",
		Digest: "sha256:1111",
		Unpack: UnpackTarGz,
		Target: "/plugins",
		Path:   writeTarGz(t, map[string]string{"lib/a.jar": "aaa"}),
	}}
	plat := Platform{OS: "linux", Architecture: "amd64"}

	first, err := AssembleAs(nil, inputs, Config{}, plat, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	second, err := AssembleAs(nil, inputs, Config{}, plat, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	d1, _ := first.Digest()
	d2, _ := second.Digest()
	if d1 != d2 {
		t.Fatalf("provenance annotations made assembly non-deterministic: %s vs %s", d1, d2)
	}

	mf, err := first.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if _, ok := mf.Annotations["org.opencontainers.image.created"]; ok {
		t.Error("a build timestamp in the manifest would make two identical specs produce two digests")
	}
}
