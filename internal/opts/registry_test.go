package opts

import (
	"reflect"
	"testing"
)

// TestSplitList — the blank-dropping is the part that matters. This list is user input from a
// values file, so an empty entry or stray whitespace must not become a host: an unset
// --insecure-registry must produce no hosts at all, and a []string{""} would match a repository
// whose host segment is empty and silently downgrade a push to plain HTTP.
func TestSplitList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"a.io:5000", []string{"a.io:5000"}},
		{" a.io , b.io ", []string{"a.io", "b.io"}},
		{"a.io,,b.io,", []string{"a.io", "b.io"}},
	} {
		if got := SplitList(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SplitList(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}
