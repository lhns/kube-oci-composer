package opts

import (
	"reflect"
	"testing"
)

// TestSplitList pins the blank-dropping: an unset --insecure-registry must yield no hosts, since
// []string{""} would match an empty host segment and downgrade a push to plain HTTP.
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
