package oci

import (
	"strconv"
	"strings"
	"testing"
)

// TestProvenanceSurvivesTheObject covers threat-model gap R1: the record of what produced an
// artifact is in the artifact, not only in the object's status.
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
	if got, want := mf.Annotations[AnnotationAssemblyVersion], strconv.Itoa(AssemblyVersion); got != want {
		t.Errorf("assembly-version annotation is %q, want the current AssemblyVersion %q", got, want)
	}
	// Absent, not empty, for a scratch artifact.
	if _, ok := mf.Annotations[AnnotationBase]; ok {
		t.Errorf("a scratch artifact must not claim a base: %v", mf.Annotations)
	}
}

// TestProvenanceRecordsTheRevisionRatherThanTheTarball: a Flux tarball's digest moves when
// source-controller re-packs; the revision says what produced the artifact.
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

// TestProvenanceKeepsSpecOrder: a later layer overwrites an earlier one, so order is semantic.
func TestProvenanceKeepsSpecOrder(t *testing.T) {
	ann := provenanceAnnotations(nil, []LayerInput{
		{Name: "zzz", Digest: "sha256:1"},
		{Name: "aaa", Digest: "sha256:2"},
	})
	if got := ann[AnnotationSources]; got != "zzz=sha256:1 aaa=sha256:2" {
		t.Errorf("sources annotation is %q, want spec order", got)
	}
}

// TestProvenanceIsDeterministic: output digest = f(spec) (ADR 0016), so no runtime observation
// (not even org.opencontainers.image.created) may appear.
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
