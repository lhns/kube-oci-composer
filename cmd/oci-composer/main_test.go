package main

import (
	"testing"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// TestTheRenamedRetentionFlagStillAnswersToItsOldName: --gc-keep-builds, renamed to --keep-builds,
// must keep working so an old values file does not crash-loop on an unknown flag.
func TestTheRenamedRetentionFlagStillAnswersToItsOldName(t *testing.T) {
	const def = ociv1alpha1.DefaultHistoryLimit

	for _, tc := range []struct {
		name             string
		keep, deprecated int
		want             int
	}{
		{"neither set", def, 0, def},
		{"only the new name", 25, 0, 25},
		{"only the old name still works", def, 25, 25},
		{"both set, the new name wins", 25, 7, 25},
		{"the old name may lower it below the default", def, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveKeepBuilds(tc.keep, tc.deprecated); got != tc.want {
				t.Errorf("effectiveKeepBuilds(%d, %d) = %d, want %d",
					tc.keep, tc.deprecated, got, tc.want)
			}
		})
	}
}
