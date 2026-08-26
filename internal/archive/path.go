package archive

import (
	"path"
	"strings"
)

// Placement is where one archive entry lands.
type Placement struct {
	// Dest is the entry's path relative to the destination root.
	Dest string
	// Selected is whether to materialise it. False for an entry outside the subpath, for the
	// subpath directory itself, and for anything stripping consumed entirely.
	Selected bool
	// InSubpath is whether the entry lies within Subpath at all. It differs from Selected for the
	// subpath directory entry, which proves the subpath exists but contributes no file -- which is
	// how a caller tells "subpath selected nothing" from "subpath selected an empty directory".
	InSubpath bool
	// Survived is whether the entry had more components than Strip removes. A caller that sees no
	// survivors at all knows stripping emptied the archive, rather than the subpath having missed.
	Survived bool
}

// Mapping decides where entries land: how many leading path components to remove, and which
// subdirectory to take.
//
// ONE implementation, used by the composer's layer assembler and the builder's context fetcher
// alike. ADR 0023 required exactly that and said why: "Two copies of that check would mean the next
// hardening reaching one of them." A second copy was written anyway, the next change to path
// handling reached one of them, and every ImageBuild with a sourceRef context broke. ADR 0045.
//
// Deliberately NOT the place for safety policy, which the two sinks differ on for recorded reasons:
// the composer rebases an absolute entry where the builder refuses it, and the composer keeps a
// symlink verbatim as inert layer data where the builder refuses one that escapes the tree. Those
// depend on whether anything is written to a filesystem. Where an entry LANDS does not.
type Mapping struct {
	// Strip is how many leading path components to remove, before Subpath is considered.
	Strip int
	// prefix is the cleaned Subpath, empty for the whole archive.
	prefix string
}

// NewMapping cleans the subpath once, so every entry is matched against the same form.
func NewMapping(strip int, subpath string) Mapping {
	prefix := strings.Trim(path.Clean("/"+subpath), "/")
	if prefix == "." {
		prefix = ""
	}
	return Mapping{Strip: strip, prefix: prefix}
}

// Subpath returns the cleaned subpath, for callers that report it in an error.
func (m Mapping) Subpath() string { return m.prefix }

// Map places one entry.
//
// name must already be a cleaned, forward-slash path relative to the archive root. Normalising
// separators is the caller's job: a backslash is a legal filename character in a tar and a path
// separator in a zip written on Windows, and only the caller knows which it is holding.
//
// STRIP FIRST, THEN SUBPATH. `subpath` names a path in the tree as it will be seen, not as the
// archive happened to store it -- otherwise a spec would have to describe a layout it also asked to
// have removed.
func (m Mapping) Map(name string) Placement {
	clean := strings.TrimPrefix(path.Clean(name), "./")
	if clean == "." || clean == "/" || clean == "" {
		// The archive root. Flux artifacts carry a bare "." entry; it contributes nothing.
		return Placement{}
	}

	for range m.Strip {
		_, rest, ok := strings.Cut(clean, "/")
		if !ok {
			// Shallower than the strip depth, so nothing of it remains. Not a survivor, which is
			// what lets the caller refuse a strip that emptied the whole archive rather than
			// producing a silently empty tree.
			return Placement{}
		}
		clean = rest
	}
	if clean == "" {
		return Placement{}
	}

	if m.prefix == "" {
		return Placement{Dest: clean, Selected: true, InSubpath: true, Survived: true}
	}
	if clean == m.prefix {
		// The subpath directory itself: proof it exists, but its contents are what was asked for.
		return Placement{InSubpath: true, Survived: true}
	}
	rest, ok := strings.CutPrefix(clean, m.prefix+"/")
	if !ok || rest == "" {
		return Placement{Survived: true}
	}
	return Placement{Dest: rest, Selected: true, InSubpath: true, Survived: true}
}
