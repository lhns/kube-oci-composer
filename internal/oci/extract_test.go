package oci

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

// Tests for the path handling every archive format shares (the collector): traversal refusal,
// absolute-name de-rooting, and subpath selection. Driven through tar, whose writer can emit any
// entry name.

// tarWithNames builds a tar containing exactly the given entry names, verbatim and in order.
func tarWithNames(t *testing.T, names ...string) *tar.Reader {
	t.Helper()
	files := make([]tarFile, 0, len(names))
	for _, name := range names {
		files = append(files, tarFile{name: name, body: "x"})
	}
	return tar.NewReader(bytes.NewReader(buildTar(t, files)))
}

// TestExtractRefusesTraversal: an archive trying to escape the target is refused, not sanitised,
// so the attempt is not hidden.
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
			// With a target, so an escape leaves somewhere it was confined to.
			_, err := extractTar(tarWithNames(t, entry), "opt/vendor", "", 0)
			if err == nil {
				t.Fatalf("entry %q was accepted; it escapes the target directory", entry)
			}
			if !strings.Contains(err.Error(), "escapes the target directory") {
				t.Errorf("entry %q: error %q does not name the escape", entry, err)
			}
		})
	}
}

// TestExtractRefusesTraversalWithoutATarget: the check must not depend on having a target.
func TestExtractRefusesTraversalWithoutATarget(t *testing.T) {
	for _, entry := range []string{"../etc/passwd", ".."} {
		if _, err := extractTar(tarWithNames(t, entry), "", "", 0); err == nil {
			t.Errorf("entry %q was accepted with no target", entry)
		}
	}
}

// TestExtractDeRootsAbsoluteNames: an absolute name is rebased under the target, a deliberate
// asymmetry with ".." (see collector.rebase).
func TestExtractDeRootsAbsoluteNames(t *testing.T) {
	entries, err := extractTar(tarWithNames(t, "/etc/passwd"), "opt", "", 0)
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if _, ok := byName(entries)["opt/etc/passwd"]; !ok {
		t.Errorf("absolute name was not de-rooted under the target, got %v", entries)
	}
}

// TestExtractSubpathCannotEscape: a spec-supplied subpath is cleaned before use, so at worst it
// selects nothing.
func TestExtractSubpathCannotEscape(t *testing.T) {
	// "../x" cleans to "x", which the archive does contain, so this selects x/ normally.
	entries, err := extractTar(tarWithNames(t, "x/a.txt"), "opt", "../x", 0)
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	for name := range byName(entries) {
		if !strings.HasPrefix(name, "opt") {
			t.Errorf("entry %q landed outside the target", name)
		}
	}

	// A subpath that resolves to nothing present is the existing terminal error, not an escape.
	if _, err := extractTar(tarWithNames(t, "x/a.txt"), "opt", "../nope", 0); err == nil {
		t.Error("a subpath matching nothing was accepted")
	}
}
