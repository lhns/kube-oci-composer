package oci

import (
	"archive/tar"
	"io"
	"strings"
	"testing"
)

// entriesOf reads back every header from a single-layer image, so tests can assert on what
// actually landed in the tar rather than on the digest alone.
func entriesOf(t *testing.T, inputs []LayerInput, cfg Config) []*tar.Header {
	t.Helper()
	img, err := Assemble(nil, inputs, cfg, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("layers: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
	rc, err := layers[0].Uncompressed()
	if err != nil {
		t.Fatalf("uncompressed: %v", err)
	}
	defer rc.Close()

	var out []*tar.Header
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		out = append(out, hdr)
	}
	return out
}

// TestRemoveEmitsWhiteouts — OCI expresses deletion as a ".wh." sibling. Getting the name or the
// directory wrong produces a layer that silently deletes nothing.
func TestRemoveEmitsWhiteouts(t *testing.T) {
	entries := entriesOf(t, []LayerInput{{
		Name:   "prune",
		Remove: []string{"/opt/kafka/libs/old.jar", "/etc/motd"},
	}}, Config{})

	want := map[string]bool{
		"opt/kafka/libs/.wh.old.jar": false,
		"etc/.wh.motd":               false,
	}
	for _, e := range entries {
		if _, ok := want[e.Name]; ok {
			want[e.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			var got []string
			for _, e := range entries {
				got = append(got, e.Name)
			}
			t.Fatalf("no whiteout %q; layer contains %v", name, got)
		}
	}
}

// TestRemoveRefusesTheRoot — a whiteout of "/" would hide the entire base, which is never what
// anyone meant to write.
func TestRemoveRefusesTheRoot(t *testing.T) {
	for _, p := range []string{"/", "", "/."} {
		if _, err := Assemble(nil, []LayerInput{{Name: "bad", Remove: []string{p}}},
			Config{}, t.TempDir()); err == nil {
			t.Fatalf("accepted a removal of %q", p)
		}
	}
}

// TestRemoveIsDeterministic — the same removals must produce the same layer, or the short-circuit
// would rebuild forever.
func TestRemoveIsDeterministic(t *testing.T) {
	mk := func() string {
		img, err := Assemble(nil, []LayerInput{{
			Name: "prune", Remove: []string{"/a/one", "/b/two", "/c/three"},
		}}, Config{}, t.TempDir())
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		d, _ := img.Digest()
		return d.String()
	}
	first := mk()
	for i := 0; i < 3; i++ {
		if mk() != first {
			t.Fatal("removals are not deterministic")
		}
	}
}

// TestOwnershipIsApplied — content is normally read rather than written, so root-owned is the
// default; this is for the case where a process must own what it reads.
func TestOwnershipIsApplied(t *testing.T) {
	src := writeTarGz(t, map[string]string{"lib/a.jar": "aaa"})

	entries := entriesOf(t, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Target: "/plugins",
		UID: 1001, GID: 1002,
	}}, Config{})

	var checked int
	for _, e := range entries {
		if e.Uid != 1001 || e.Gid != 1002 {
			t.Fatalf("entry %q is owned by %d:%d, want 1001:1002", e.Name, e.Uid, e.Gid)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no entries were checked")
	}
}

// TestOwnershipDefaultsToRoot — the common case, and the one the digest was pinned against.
func TestOwnershipDefaultsToRoot(t *testing.T) {
	src := writeTarGz(t, map[string]string{"lib/a.jar": "aaa"})
	for _, e := range entriesOf(t, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Target: "/plugins",
	}}, Config{}) {
		if e.Uid != 0 || e.Gid != 0 {
			t.Fatalf("entry %q defaulted to %d:%d, want 0:0", e.Name, e.Uid, e.Gid)
		}
	}
}

// TestModeOverrideIsApplied — and applies to files and directories separately, since a directory
// needs the execute bit to be traversable and a file usually should not have it.
func TestModeOverrideIsApplied(t *testing.T) {
	src := writeTarGz(t, map[string]string{"lib/a.jar": "aaa"})

	entries := entriesOf(t, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Target: "/plugins",
		FileMode: 0o600, DirMode: 0o700,
	}}, Config{})

	var files, dirs int
	for _, e := range entries {
		switch e.Typeflag {
		case tar.TypeReg:
			if e.Mode != 0o600 {
				t.Fatalf("file %q has mode %o, want 600", e.Name, e.Mode)
			}
			files++
		case tar.TypeDir:
			if e.Mode != 0o700 {
				t.Fatalf("dir %q has mode %o, want 700", e.Name, e.Mode)
			}
			dirs++
		}
	}
	if files == 0 || dirs == 0 {
		t.Fatalf("checked %d files and %d dirs; the assertion proves nothing", files, dirs)
	}
}

// TestModeDefaultsToNormalised — without an override, permissions are normalised so that whoever
// packed the upstream archive cannot vary the output digest through bits nobody looks at.
func TestModeDefaultsToNormalised(t *testing.T) {
	src := writeTarGz(t, map[string]string{"lib/a.jar": "aaa"})
	for _, e := range entriesOf(t, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Target: "/plugins",
	}}, Config{}) {
		switch e.Typeflag {
		case tar.TypeReg:
			if e.Mode != 0o644 {
				t.Fatalf("file %q has mode %o, want 644", e.Name, e.Mode)
			}
		case tar.TypeDir:
			if e.Mode != 0o755 {
				t.Fatalf("dir %q has mode %o, want 755", e.Name, e.Mode)
			}
		}
	}
}

// TestSubpathStripsThePrefix — a release tarball usually wraps everything in a version-named
// directory. Selecting it must place its CONTENTS at the target, not the directory itself.
func TestSubpathStripsThePrefix(t *testing.T) {
	src := writeTarGz(t, map[string]string{
		"core-1.1.1/lib/a.jar": "aaa",
		"core-1.1.1/README":    "readme",
		"unrelated/b.jar":      "bbb",
	})

	entries := entriesOf(t, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Subpath: "core-1.1.1", Target: "/plugins",
	}}, Config{})

	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	joined := strings.Join(names, " ")

	if !strings.Contains(joined, "plugins/lib/a.jar") {
		t.Fatalf("the subpath prefix was not stripped: %v", names)
	}
	if strings.Contains(joined, "core-1.1.1") {
		t.Fatalf("the subpath directory itself leaked into the layer: %v", names)
	}
	if strings.Contains(joined, "unrelated") {
		t.Fatalf("content outside the subpath was included: %v", names)
	}
}

// TestSubpathMatchingNothingIsAnError — a typo would otherwise produce an empty layer, and the
// workload would start with files missing for no visible reason.
func TestSubpathMatchingNothingIsAnError(t *testing.T) {
	src := writeTarGz(t, map[string]string{"core-1.1.1/lib/a.jar": "aaa"})

	if _, err := Assemble(nil, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Subpath: "core-2.0.0", Target: "/plugins",
	}}, Config{}, t.TempDir()); err == nil {
		t.Fatal("a subpath matching nothing produced a layer instead of an error")
	}
}

// TestConfigSurfaceIsStamped — the fields that make a composed image runnable.
func TestConfigSurfaceIsStamped(t *testing.T) {
	src := writeTarGz(t, map[string]string{"a": "1"})

	img, err := Assemble(nil, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Target: "/x",
	}}, Config{
		User:         "1001",
		WorkingDir:   "/opt/kafka",
		ExposedPorts: []string{"9092/tcp"},
		Volumes:      []string{"/data"},
		StopSignal:   "SIGTERM",
		Env:          []string{"KAFKA_HOME=/opt/kafka"},
	}, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cf.Config.User != "1001" {
		t.Errorf("user %q", cf.Config.User)
	}
	if cf.Config.WorkingDir != "/opt/kafka" {
		t.Errorf("workingDir %q", cf.Config.WorkingDir)
	}
	if _, ok := cf.Config.ExposedPorts["9092/tcp"]; !ok {
		t.Errorf("exposedPorts %v", cf.Config.ExposedPorts)
	}
	if _, ok := cf.Config.Volumes["/data"]; !ok {
		t.Errorf("volumes %v", cf.Config.Volumes)
	}
	if cf.Config.StopSignal != "SIGTERM" {
		t.Errorf("stopSignal %q", cf.Config.StopSignal)
	}
}

// TestInheritWithoutABaseIsAnError — nothing to inherit from, and an empty config would leave a
// non-runnable image with no explanation.
func TestInheritWithoutABaseIsAnError(t *testing.T) {
	src := writeTarGz(t, map[string]string{"a": "1"})

	if _, err := Assemble(nil, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Target: "/x",
	}}, Config{Inherit: true}, t.TempDir()); err == nil {
		t.Fatal("config.inherit was accepted without a base")
	}
}
