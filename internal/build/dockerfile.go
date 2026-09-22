package build

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Dockerfile inspection, for the one content rule this controller enforces: every FROM is pinned
// by digest (ADR 0002). The Dockerfile is not in the spec, so CEL cannot check it.
//
// A scanner, not a parser: it finds FROM instructions and leaves Dockerfile semantics to BuildKit.

// CheckPinnedBases refuses a Dockerfile whose external base images are not pinned by digest,
// listing every offending reference at once.
func CheckPinnedBases(r io.Reader) error {
	// Stage aliases, so FROM naming an earlier stage is not taken for a registry reference.
	stages := map[string]bool{}
	var unpinned []string

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var continued string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		// Join continuations, so a FROM split across lines is seen whole.
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
			// An earlier stage.
		case strings.EqualFold(ref, "scratch"):
			// The empty base.
		case strings.Contains(ref, "$"):
			// ARG-substituted: refused, since resolving it would mean reimplementing Dockerfile
			// variable semantics.
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
