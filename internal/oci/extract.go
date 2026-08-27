package oci

import (
	"fmt"
	"path"
	"strings"

	"github.com/lhns/kube-oci-composer/internal/archive"
)

// Archive-independent extraction.
//
// Formats differ in how they describe an entry — a tar has a typeflag, a zip infers everything from
// a mode word and a trailing slash, a deb wraps a tar in an ar container. They must NOT differ in
// where an entry is allowed to land, which is what this file owns: traversal refusal, subpath
// selection, rebasing under the target, and directory de-duplication.
//
// It is one type rather than a set of helpers because the subpath check is stateful: "this subpath
// matched nothing" can only be decided after the whole archive has been walked, and a format that
// forgot to make that check would silently produce an empty layer.

// collector accumulates the entries one input contributes.
type collector struct {
	// target is the cleaned, relative destination inside the image. Empty is the image root.
	target string
	// walk decides where an entry lands and tallies what the archive proved. SHARED with the
	// builder's fetcher, placement and refusal alike -- see archive.Walk and ADR 0045.
	walk *archive.Walk
	// dirs de-duplicates directory entries, whether they came from the archive or were synthesised.
	dirs    map[string]bool
	entries []tarEntry
}

func newCollector(target, subpath string, strip int) *collector {
	return &collector{
		target: target,
		walk:   archive.NewWalk(strip, subpath),
		dirs:   make(map[string]bool),
	}
}

// rebase maps an archive-declared name onto its place in the layer.
//
// ok is false when the entry contributes nothing — outside the subpath, the subpath directory
// itself, or the archive root. err is a traversal attempt, refused rather than sanitised.
//
// name must already use forward slashes; see archive.Mapping.Map for why that is the caller's job.
func (c *collector) rebase(name string) (dest string, ok bool, err error) {
	clean := path.Clean(name)

	// Refuse anything that walks out of the target. Checked on the cleaned RELATIVE form, so
	// "a/../../etc" is caught after Clean collapses it to "../etc". path.Clean already swallows
	// ".." at an absolute root, so an absolute name is de-rooted below instead.
	//
	// The bare ".." is spelled out separately because it has no "../" prefix, and an earlier
	// version that only tested the prefix let it land one level above the target.
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("archive entry %q escapes the target directory", name)
	}

	// An absolute name is rebased rather than refused. Deliberate asymmetry with the above:
	// "/etc/passwd" names a path inside the archive's own idea of a root and has an obvious
	// harmless reading, whereas ".." has none.
	clean = strings.TrimPrefix(clean, "/")

	// Everything from here -- strip depth, subpath selection, the refusals in done -- is shared.
	place := c.walk.Map(clean)
	if !place.Selected {
		return "", false, nil
	}

	if c.target == "" {
		return place.Dest, true, nil
	}
	return path.Join(c.target, place.Dest), true, nil
}

// addDir records a directory, ignoring a repeat.
func (c *collector) addDir(name string) {
	if c.dirs[name] {
		return
	}
	c.dirs[name] = true
	c.entries = append(c.entries, tarEntry{name: name, mode: 0o755, dir: true})
}

// addFile records a regular file, synthesising the directories leading to it.
//
// The parents are synthesised because many archives omit directory entries entirely, and a tar
// whose files have no parent directories is not reliably extractable.
func (c *collector) addFile(name string, mode int64, body []byte) {
	for _, d := range parentDirs(name) {
		if !c.dirs[d.name] {
			c.dirs[d.name] = true
			c.entries = append(c.entries, d)
		}
	}
	c.entries = append(c.entries, tarEntry{name: name, mode: mode, body: body})
}

// addSymlink records a symlink, with the target kept verbatim.
//
// Targets are neither resolved nor validated: nothing here writes to a filesystem, so a link is
// inert data until a runtime resolves it inside the consuming container's own rootfs. Parent
// directories are deliberately not synthesised, unlike addFile — adding them would change the bytes
// of every existing layer containing a symlink, and so require an AssemblyVersion bump.
func (c *collector) addSymlink(name, link string) {
	c.entries = append(c.entries, tarEntry{name: name, mode: 0o777, link: link})
}

// done returns the collected entries, unless the selection contributed nothing.
func (c *collector) done() ([]tarEntry, error) {
	if err := c.walk.Err(); err != nil {
		return nil, err
	}
	return c.entries, nil
}

// normaliseMode keeps the executable bit and discards the rest, so that permissions from
// whoever built the upstream archive cannot vary the digest in surprising ways.
func normaliseMode(mode int64) int64 {
	if mode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// parentDirs returns the directory entries leading to name.
func parentDirs(name string) []tarEntry {
	var out []tarEntry
	dir := path.Dir(name)
	if dir == "." || dir == "/" || dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		out = append(out, tarEntry{name: strings.Join(parts[:i+1], "/"), mode: 0o755, dir: true})
	}
	return out
}
