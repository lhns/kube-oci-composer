package v1alpha1

import "testing"

// TestRevisionMatches — the short form exists because a generator knows the tag it asked for and
// not the commit it resolved to. If pinning required the full revision, the thing best placed to
// set this field could not.
func TestRevisionMatches(t *testing.T) {
	const got = "v0.6.8@sha1:b739efb5"

	for _, tc := range []struct {
		name string
		want string
		got  string
		ok   bool
	}{
		{"unset consumes whatever is published", "", got, true},
		{"ref half matches any commit", "v0.6.8", got, true},
		{"full revision matches itself", got, got, true},
		{"a different tag does not match", "v0.6.5", got, false},
		{"a different commit does not match", "v0.6.8@sha1:0000000", got, false},
		{"a ref is not a prefix match", "v0.6", got, false},
		{"a ref must be the whole ref half", "0.6.8", got, false},
		{"branch revisions work the same way", "main", "main@sha1:abcd", true},
		{"a revision with no commit still compares", "main", "main", true},
		{"nothing published yet does not satisfy a pin", "v0.6.8", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if RevisionMatches(tc.want, tc.got) != tc.ok {
				t.Errorf("RevisionMatches(%q, %q) = %v, want %v", tc.want, tc.got, !tc.ok, tc.ok)
			}
		})
	}
}
