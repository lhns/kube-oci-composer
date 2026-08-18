package build

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Dockerfile inspection, for the one rule this controller enforces on a build's content.
//
// A floating FROM is the single largest source of "same commit, different image": the context
// digest is unchanged, the spec is unchanged, the input hash is unchanged, and the base moved
// underneath all of it. Refusing it is the direct analogue of ADR 0002's "every input is
// content-addressed, there are no exceptions anywhere in the API", applied at the one place it
// can be applied — the Dockerfile is not in the spec, so CEL cannot see it and the check has to
// live here.
//
// This is a scanner, not a parser. It understands enough to find FROM instructions and nothing
// else, because everything else is BuildKit's job and a second opinion about Dockerfile semantics
// is a source of disagreement rather than safety.

// CheckPinnedBases refuses a Dockerfile whose external base images are not pinned by digest.
//
// Returns every offending reference rather than the first, so a Dockerfile with three floating
// FROMs takes one edit to fix rather than three round trips.
func CheckPinnedBases(r io.Reader) error {
	// Aliases this Dockerfile defines, so a later FROM naming an earlier stage is not mistaken
	// for an unpinned registry reference.
	stages := map[string]bool{}
	var unpinned []string

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var continued string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		// Join continuations before looking at the instruction, so a FROM split across lines is
		// still seen as one.
		if continued != "" {
			line = continued + " " + line
			continued = ""
		}
		if head, ok := strings.CutSuffix(line, "\\"); ok {
			continued = strings.TrimSpace(head)
			continue
		}

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}

		ref := fields[1]

		// "FROM x AS builder" defines a stage. AS and its name can only be the last two fields.
		if n := len(fields); n >= 4 && strings.EqualFold(fields[n-2], "AS") {
			stages[strings.ToLower(fields[n-1])] = true
		}

		switch {
		case stages[strings.ToLower(ref)]:
			// A reference to an earlier stage in this same Dockerfile. Nothing to pin.
		case strings.EqualFold(ref, "scratch"):
			// The empty base. Not a registry reference at all.
		case strings.Contains(ref, "$"):
			// An ARG-substituted base. Refused rather than resolved: substituting it here would
			// mean reimplementing Dockerfile variable semantics, and guessing wrong would let an
			// unpinned base through while claiming otherwise.
			unpinned = append(unpinned, ref)
		case strings.Contains(ref, "@sha256:"):
			// Pinned.
		default:
			unpinned = append(unpinned, ref)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading the Dockerfile: %w", err)
	}

	if len(unpinned) > 0 {
		return fmt.Errorf("every FROM must be pinned by digest; these are not: %q — "+
			"pin with repo:tag@sha256:… so that an unchanged spec cannot silently build on a "+
			"different base", unpinned)
	}
	return nil
}
