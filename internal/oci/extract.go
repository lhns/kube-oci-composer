package oci

import (
	"fmt"
	"path"
	"strings"

	"github.com/lhns/kube-oci-composer/internal/archive"
)

// Archive-independent extraction. Formats differ in how they describe an entry, but must not differ
// in where it may land: traversal refusal, subpath selection, rebasing and directory
// de-duplication live here. A stateful type, because "the subpath matched nothing" is only known
// after the whole archive has been walked.

// collector accumulates the entries one input contributes.
type collector struct {
	// target is the cleaned, relative destination inside the image. Empty is the image root.
	target string
	// walk decides where an entry lands; shared with the builder's fetcher (ADR 0045).
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
// ok is false when the entry contributes nothing (outside the subpath, the subpath directory
// itself, or the archive root). err is a refused traversal attempt. name must already use forward
// slashes; see archive.Mapping.Map.
func (c *collector) rebase(name string) (dest string, ok bool, err error) {
	clean := path.Clean(name)

	// Refuse anything that walks out of the target, checked after Clean ("a/../../etc" becomes
	// "../etc"). The bare ".." has no "../" prefix, so it is tested separately.
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("archive entry %q escapes the target directory", name)
	}

	// An absolute name is rebased rather than refused: unlike "..", it has an obvious harmless
	// reading relative to the archive's root.
	clean = strings.TrimPrefix(clean, "/")

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
// Many archives omit directory entries, and a tar without them is not reliably extractable.
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
// Targets are neither resolved nor validated: nothing here writes to a filesystem, and a runtime
// resolves the link inside the container's own rootfs. Unlike addFile, parents are not synthesised;
// doing so would change existing layers' bytes and need an AssemblyVersion bump.
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

// normaliseMode keeps the executable bit and discards the rest, so upstream permissions cannot
// vary the digest.
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
