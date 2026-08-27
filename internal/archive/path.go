package archive

import (
	"fmt"
	"path"
	"strings"
)

// Placement is where one archive entry lands.
//
// Dest and Selected are the answer; the two unexported fields are the evidence Walk tallies to tell
// "selected nothing" apart from "selected an empty directory".
type Placement struct {
	// Dest is the entry's path relative to the destination root.
	Dest string
	// Selected is whether to materialise it. False for an entry outside the subpath, for the
	// subpath directory itself, and for anything stripping consumed entirely.
	Selected bool
	// inSubpath is whether the entry lies within the subpath at all. It differs from Selected for
	// the subpath directory entry, which proves the subpath exists but contributes no file.
	inSubpath bool
	// survived is whether the entry had more components than Strip removes. No survivors at all
	// means stripping emptied the archive, rather than the subpath having missed.
	survived bool
}

// Mapping decides where entries land: how many leading path components to remove, and which
// subdirectory to take. ONE implementation, shared by the composer's assembler and the builder's
// fetcher -- ADR 0045.
//
// Deliberately NOT the place for safety policy: the two sinks differ there for recorded reasons,
// because it depends on whether anything is written to a filesystem. Where an entry lands does not.
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

// Map places one entry.
//
// name must use forward slashes already. Normalising separators is the caller's job: a backslash is
// a legal filename character in a tar and a separator in a zip written on Windows, and only the
// caller knows which it holds.
//
// STRIP FIRST, THEN SUBPATH -- `subpath` names the tree as it will be seen, not as the archive
// stored it, so a spec never has to describe a layout it also asked to remove.
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
		return Placement{Dest: clean, Selected: true, inSubpath: true, survived: true}
	}
	if clean == m.prefix {
		// The subpath directory itself: proof it exists, but its contents are what was asked for.
		return Placement{inSubpath: true, survived: true}
	}
	rest, ok := strings.CutPrefix(clean, m.prefix+"/")
	if !ok || rest == "" {
		return Placement{survived: true}
	}
	return Placement{Dest: rest, Selected: true, inSubpath: true, survived: true}
}

// Walk is a Mapping plus the tally needed to refuse a selection that emptied the archive. Shared
// for the same reason Mapping is: both sinks had grown their own counters and their own copies of
// these two errors.
type Walk struct {
	mapping   Mapping
	declared  string
	matched   bool
	survivors int
}

// NewWalk starts a walk. declared is the subpath as written, so an error quotes what the user wrote.
func NewWalk(strip int, subpath string) *Walk {
	return &Walk{mapping: NewMapping(strip, subpath), declared: subpath}
}

// Map places one entry and records what it proved.
func (w *Walk) Map(name string) Placement {
	place := w.mapping.Map(name)
	if place.survived {
		w.survivors++
	}
	if place.inSubpath {
		w.matched = true
	}
	return place
}

// Err reports a selection that contributed nothing. A silently empty tree surfaces as a broken
// build somewhere else entirely, so a typo stalls here instead.
func (w *Walk) Err() error {
	if w.mapping.Strip > 0 && w.survivors == 0 {
		return fmt.Errorf("stripComponents %d removed every entry in the archive", w.mapping.Strip)
	}
	if w.mapping.prefix != "" && !w.matched {
		return fmt.Errorf("subpath %q is not present in the archive", w.declared)
	}
	return nil
}
