package oci

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

// Tests for the path handling every archive format shares: traversal refusal, absolute-name
// de-rooting, and subpath selection.
//
// They drive it through the tar path because that is the format with a writer flexible enough to
// emit any entry name, including the ones a well-behaved packer never would. What they actually
// pin is the collector, which the zip path reaches too — so a regression here is a regression in
// both formats, which is the reason this file exists separately from assemble_test.go.

// tarWithNames builds an uncompressed tar containing exactly the given entry names, bypassing the
// normalisation writeTarGz's map-of-files shape would impose. A name is written verbatim, which is
// the whole point: the guard under test runs on what the archive claims, not on what is sensible.
func tarWithNames(t *testing.T, names ...string) *tar.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		body := "x"
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("writing header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("writing body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	return tar.NewReader(&buf)
}

// TestExtractRefusesTraversal — this guard is the only thing standing between a malicious archive
// and an entry placed outside the layer's target. Sanitising is deliberately NOT the behaviour:
// an archive trying to escape is refused, because quietly correcting it hides the attempt.
func TestExtractRefusesTraversal(t *testing.T) {
	cases := map[string]string{
		"parent prefix":     "../etc/passwd",
		"parent mid-path":   "a/../../etc/passwd",
		"bare parent":       "..",
		"parent as subdir":  "x/../..",
		"deep parent chain": "../../../../etc/passwd",
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			// target is set, since that is the case where an escape actually leaves somewhere it
			// was confined to.
			_, err := extractTar(tarWithNames(t, entry), "opt/vendor", "")
			if err == nil {
				t.Fatalf("entry %q was accepted; it escapes the target directory", entry)
			}
			if !strings.Contains(err.Error(), "escapes the target directory") {
				t.Errorf("entry %q: error %q does not name the escape", entry, err)
			}
		})
	}
}

// TestExtractRefusesTraversalWithoutATarget — an escape is still an escape when the target is the
// image root. The check must not be conditional on having somewhere to escape FROM, or an artifact
// with no target inherits a weaker guard than one with.
func TestExtractRefusesTraversalWithoutATarget(t *testing.T) {
	for _, entry := range []string{"../etc/passwd", ".."} {
		if _, err := extractTar(tarWithNames(t, entry), "", ""); err == nil {
			t.Errorf("entry %q was accepted with no target", entry)
		}
	}
}

// TestExtractDeRootsAbsoluteNames — an absolute entry name is rebased under the target rather than
// refused, which is a deliberate asymmetry with traversal: "/etc/passwd" names a location inside
// the archive's own idea of a root, so it has an obvious and harmless reading, whereas ".." does
// not. Pinned here so it reads as a decision rather than an oversight.
func TestExtractDeRootsAbsoluteNames(t *testing.T) {
	entries, err := extractTar(tarWithNames(t, "/etc/passwd"), "opt", "")
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if _, ok := byName(entries)["opt/etc/passwd"]; !ok {
		t.Errorf("absolute name was not de-rooted under the target, got %v", entries)
	}
}

// TestExtractSubpathCannotEscape — subpath is spec-supplied rather than archive-supplied, so it is
// not an attack so much as a typo, but it must not become a way around the target either. Clean
// resolves it before it is used as a prefix, so the worst case is selecting nothing.
func TestExtractSubpathCannotEscape(t *testing.T) {
	// "../x" cleans to "x", which the archive does contain, so this selects x/ normally.
	entries, err := extractTar(tarWithNames(t, "x/a.txt"), "opt", "../x")
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	for name := range byName(entries) {
		if !strings.HasPrefix(name, "opt") {
			t.Errorf("entry %q landed outside the target", name)
		}
	}

	// A subpath that resolves to nothing present is the existing terminal error, not an escape.
	if _, err := extractTar(tarWithNames(t, "x/a.txt"), "opt", "../nope"); err == nil {
		t.Error("a subpath matching nothing was accepted")
	}
}
