package archive

import (
	"strings"
	"testing"

	"github.com/lhns/kube-oci-composer/internal/archive/archivetest"
)

// TestMappingPlacesEveryCase runs the shared table against the mapping itself.
func TestMappingPlacesEveryCase(t *testing.T) {
	for _, tc := range archivetest.PathCases() {
		t.Run(tc.Name, func(t *testing.T) {
			got := NewMapping(tc.Strip, tc.Subpath).Map(tc.Entry)
			if got.Dest != tc.Dest {
				t.Errorf("Dest = %q, want %q", got.Dest, tc.Dest)
			}
			if got.Selected != (tc.Dest != "") {
				t.Errorf("Selected = %v for Dest %q", got.Selected, got.Dest)
			}
			if got.inSubpath != tc.InSubpath {
				t.Errorf("inSubpath = %v, want %v", got.inSubpath, tc.InSubpath)
			}
		})
	}
}

// TestAWalkRefusesASelectionThatContributedNothing pins both refusals in the one place they now
// live. Neither had a test asserting its message while each sink carried its own copy.
func TestAWalkRefusesASelectionThatContributedNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		strip   int
		subpath string
		entries []string
		want    string
	}{
		{"strip deeper than every entry", 2, "", []string{"Dockerfile", "a/b"}, "stripComponents 2 removed every entry"},
		{"subpath matches nothing", 0, "ui", []string{"server/main.go"}, `subpath "ui" is not present`},
		{"the subpath directory alone is enough", 0, "ui", []string{"ui"}, ""},
		{"strip that leaves something is fine", 1, "", []string{"app-1.2.3/Dockerfile"}, ""},
		{"an empty archive with no selection is not an error", 0, "", nil, ""},
		// The error quotes the subpath AS WRITTEN, not the cleaned form, so it matches the spec.
		{"the message quotes what was written", 0, "./ui/", []string{"server/main.go"}, `subpath "./ui/" is not present`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWalk(tc.strip, tc.subpath)
			for _, e := range tc.entries {
				w.Map(e)
			}
			err := w.Err()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected a refusal containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
