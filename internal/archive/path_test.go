package archive

import (
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
			if got.InSubpath != tc.InSubpath {
				t.Errorf("InSubpath = %v, want %v", got.InSubpath, tc.InSubpath)
			}
		})
	}
}
