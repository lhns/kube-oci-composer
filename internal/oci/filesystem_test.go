package oci

import (
	"archive/tar"
	"io"
	"strings"
	"testing"
)

// entriesOf reads back every header from a single-layer image.
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

// assembleDigest returns the digest of a single-layer assembly.
func assembleDigest(t *testing.T, inputs []LayerInput, cfg Config) string {
	t.Helper()
	img, err := Assemble(nil, inputs, cfg, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return d.String()
}

// TestRemoveEmitsWhiteouts: a wrong ".wh." name or directory silently deletes nothing.
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

// TestRemoveRefusesTheRoot: a whiteout of "/" would hide the entire base.
func TestRemoveRefusesTheRoot(t *testing.T) {
	for _, p := range []string{"/", "", "/."} {
		if _, err := Assemble(nil, []LayerInput{{Name: "bad", Remove: []string{p}}},
			Config{}, t.TempDir()); err == nil {
			t.Fatalf("accepted a removal of %q", p)
		}
	}
}

// TestRemoveIsDeterministic: the same removals produce the same layer.
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

// TestOwnershipIsApplied: for a process that must own what it reads.
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

// TestOwnershipDefaultsToRoot: the common case.
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

// TestModeOverrideIsApplied: separately to files and directories, which need the execute bit.
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

// TestModeDefaultsToNormalised: without an override, upstream permissions cannot vary the digest.
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

// TestSubpathStripsThePrefix: selecting a directory places its CONTENTS at the target.
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

// TestSubpathMatchingNothingIsAnError: a typo must not produce an empty layer.
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

// TestInheritWithoutABaseIsAnError: there is nothing to inherit from.
func TestInheritWithoutABaseIsAnError(t *testing.T) {
	src := writeTarGz(t, map[string]string{"a": "1"})

	if _, err := Assemble(nil, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Target: "/x",
	}}, Config{Inherit: true}, t.TempDir()); err == nil {
		t.Fatal("config.inherit was accepted without a base")
	}
}
