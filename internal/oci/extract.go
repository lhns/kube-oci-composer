package oci

import (
	"fmt"
	"path"
	"strings"
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
	// prefix is the cleaned subpath, empty for the whole archive.
	prefix string
	// subpath is the subpath as declared, kept only so the error message quotes what was written.
	subpath string
	// matched records whether prefix selected anything at all.
	matched bool
	// dirs de-duplicates directory entries, whether they came from the archive or were synthesised.
	dirs    map[string]bool
	entries []tarEntry
}

func newCollector(target, subpath string) *collector {
	prefix := strings.Trim(path.Clean("/"+subpath), "/")
	if prefix == "." {
		prefix = ""
	}
	return &collector{
		target:  target,
		prefix:  prefix,
		subpath: subpath,
		dirs:    make(map[string]bool),
	}
}

// rebase maps an archive-declared name onto its place in the layer.
//
// ok is false when the entry contributes nothing — it is outside the subpath, or it IS the subpath
// directory, or it is the archive root. err is a traversal attempt, which is refused rather than
// sanitised: an archive trying to escape its target is not something to quietly correct.
//
// name must already use forward slashes. Normalising separators is the CALLER's job, because a
// backslash is a legal filename character in a tar and a path separator in a zip written on
// Windows, and only the caller knows which it is holding.
func (c *collector) rebase(name string) (dest string, ok bool, err error) {
	clean := path.Clean(name)

	// Refuse anything that walks out of the target. Checked on the cleaned RELATIVE form, so
	// "a/../../etc" is caught after Clean collapses it to "../etc". Note that path.Clean already
	// swallows ".." at an absolute root — "/../etc" becomes "/etc" — so an absolute name cannot
	// reach this and is de-rooted below instead.
	//
	// The bare ".." case is spelled out because it is the one an earlier version of this check
	// missed: it tested `clean == ".."` in the outer condition but then only errored on the
	// "../" prefix, which ".." does not have, so the entry survived and landed one level above
	// the target.
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("archive entry %q escapes the target directory", name)
	}

	// An absolute name is rebased rather than refused. Deliberate asymmetry with the above:
	// "/etc/passwd" names a path inside the archive's own idea of a root and has an obvious
	// harmless reading, whereas ".." has none.
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", false, nil
	}

	if c.prefix != "" {
		switch {
		case clean == c.prefix:
			c.matched = true
			return "", false, nil // the directory itself; its contents are what we want
		case strings.HasPrefix(clean, c.prefix+"/"):
			c.matched = true
			clean = strings.TrimPrefix(clean, c.prefix+"/")
		default:
			return "", false, nil
		}
	}

	if c.target == "" {
		return clean, true, nil
	}
	return path.Join(c.target, clean), true, nil
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
// Link targets are NOT resolved or validated. Nothing here writes to a filesystem — the entries
// become a tar and then a layer — so a link is inert data until a runtime resolves it inside the
// consuming container's own rootfs. Rewriting one would break the case symlinks are carried for:
// a native library shipped as a real file plus a relative link beside it.
//
// Parent directories are deliberately NOT synthesised here, matching what this code has always
// done. Adding them would change the bytes of every existing layer containing a symlink, which
// would mean bumping AssemblyVersion and rebuilding every artifact in every cluster for a
// cosmetic difference.
func (c *collector) addSymlink(name, link string) {
	c.entries = append(c.entries, tarEntry{name: name, mode: 0o777, link: link})
}

// done returns the collected entries, or an error if the subpath selected nothing.
//
// A subpath that matched nothing is a silent empty layer, and the workload then starts with files
// missing for no visible reason. Far better to stall on a typo.
func (c *collector) done() ([]tarEntry, error) {
	if c.prefix != "" && !c.matched {
		return nil, fmt.Errorf("path %q is not present in the archive", c.subpath)
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
