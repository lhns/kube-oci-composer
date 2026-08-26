// Package archivetest holds the shared truth table for where an archive entry lands.
//
// Its own package because a _test.go file cannot be imported: the composer's assembler and the
// builder's fetcher must both drive their extractors from THIS list, or the thing ADR 0045 was
// written about happens again.
package archivetest

// PathCase is one entry mapped by one Mapping.
//
// Exported so the composer's assembler drives its extractors from the same list. That is the whole
// point: ADR 0023 required one implementation of where an entry lands, a second was written anyway,
// and the two disagreed until every sourceRef build broke (ADR 0045). One table means neither side
// can quietly cover a case the other gets wrong.
type PathCase struct {
	Name    string
	Entry   string
	Strip   int
	Subpath string
	// Dest is where it lands. Empty means it contributes nothing.
	Dest string
	// InSubpath is whether it counts as proof the subpath exists.
	InSubpath bool
}

// PathCases is the shared truth table.
func PathCases() []PathCase {
	return []PathCase{
		// A Flux artifact: a bare root entry, then files at the top level.
		{"root entry contributes nothing", ".", 0, "", "", false},
		{"root file survives", "Dockerfile", 0, "", "Dockerfile", true},
		{"dot-slash prefix is cleaned", "./Dockerfile", 0, "", "Dockerfile", true},
		{"nested path is unchanged", "ui/Button.tsx", 0, "", "ui/Button.tsx", true},

		// Stripping, which only ever comes from the spec.
		{"one level removed", "app-1.2.3/Dockerfile", 1, "", "Dockerfile", true},
		{"two levels removed", "a/b/Dockerfile", 2, "", "Dockerfile", true},
		{"shallower than the strip depth", "Dockerfile", 1, "", "", false},
		{"exactly the strip depth", "app-1.2.3", 1, "", "", false},

		// Subpath, matched AFTER stripping.
		{"subpath selects", "ui/Button.tsx", 0, "ui", "Button.tsx", true},
		{"subpath excludes", "server/main.go", 0, "ui", "", false},
		{"subpath directory itself proves it exists", "ui", 0, "ui", "", true},
		{"strip then subpath", "app-1.2.3/ui/Button.tsx", 1, "ui", "Button.tsx", true},
		{"subpath named as stored is wrong after stripping", "app-1.2.3/ui/x.ts", 1, "app-1.2.3/ui", "", false},
	}
}
