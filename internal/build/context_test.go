package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// contextServer serves a gzipped tar of the given entries, the way source-controller publishes an
// artifact.
func contextServer(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("writing header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("writing body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}

	raw := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchDockerfileStripsTheWrapperDirectory — source-controller wraps an artifact in one
// top-level directory whose name is a revision nobody can predict, so a match on the whole path
// would never fire.
func TestFetchDockerfileStripsTheWrapperDirectory(t *testing.T) {
	srv := contextServer(t, map[string]string{
		"app-4f2b1c9/Dockerfile": "FROM scratch\n",
		"app-4f2b1c9/main.go":    "package main\n",
	})

	got, err := FetchDockerfile(context.Background(), srv.Client(), srv.URL, "", "Dockerfile", 1)
	if err != nil {
		t.Fatalf("FetchDockerfile: %v", err)
	}
	if string(got) != "FROM scratch\n" {
		t.Errorf("got %q", got)
	}
}

// TestFetchDockerfileHonoursSubpathAndName — a monorepo puts its Dockerfile somewhere, and the two
// fields that say where must compose.
func TestFetchDockerfileHonoursSubpathAndName(t *testing.T) {
	srv := contextServer(t, map[string]string{
		"repo-abc/Dockerfile":                "FROM wrong\n",
		"repo-abc/services/api/build.docker": "FROM right\n",
	})

	got, err := FetchDockerfile(context.Background(), srv.Client(), srv.URL, "services/api", "build.docker", 1)
	if err != nil {
		t.Fatalf("FetchDockerfile: %v", err)
	}
	if string(got) != "FROM right\n" {
		t.Errorf("got %q, want the subpath's Dockerfile", got)
	}
}

// TestFetchDockerfileMissing — a typo in spec.dockerfile must say so, rather than silently letting
// the FROM check pass over a file that was never read.
func TestFetchDockerfileMissing(t *testing.T) {
	srv := contextServer(t, map[string]string{"repo/Dockerfile": "FROM scratch\n"})

	_, err := FetchDockerfile(context.Background(), srv.Client(), srv.URL, "", "Containerfile", 1)
	if err == nil {
		t.Fatal("a missing Dockerfile was accepted")
	}
	if !strings.Contains(err.Error(), "not present in the build context") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

// TestFetchDockerfileRejectsBadStatus — a source-controller artifact that has been garbage
// collected returns 404, and that must not read as an empty Dockerfile.
func TestFetchDockerfileRejectsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	if _, err := FetchDockerfile(context.Background(), srv.Client(), srv.URL, "", "Dockerfile", 1); err == nil {
		t.Fatal("a 404 was accepted")
	}
}

// TestMatchesContextPath — the path rule, directly, at both depths.
//
// It used to try the exact match FIRST and only then skip a leading component, so it found a
// Dockerfile whatever the archive's shape. That tolerance looked helpful and was not: it kept
// finding the file while the extractor next to it emptied the context around it, which is why the
// bug in ADR 0045 presented as a BuildKit error rather than a fetch failure. One rule now, the
// same one the extractor uses.
func TestMatchesContextPath(t *testing.T) {
	cases := []struct {
		entry, want string
		strip       int
		match       bool
	}{
		// Nothing stripped: the archive's paths are the paths. This is a Flux artifact.
		{"Dockerfile", "Dockerfile", 0, true},
		{"./Dockerfile", "Dockerfile", 0, true},
		{"services/api/Dockerfile", "services/api/Dockerfile", 0, true},
		{"app-abc/Dockerfile", "Dockerfile", 0, false},

		// One component removed: a release tarball, with stripComponents: 1.
		{"app-abc/Dockerfile", "Dockerfile", 1, true},
		{"./app-abc/Dockerfile", "Dockerfile", 1, true},
		{"app-abc/services/api/Dockerfile", "services/api/Dockerfile", 1, true},
		{"app-abc/nested/Dockerfile", "Dockerfile", 1, false},
		{"app-abc/Dockerfile.dev", "Dockerfile", 1, false},

		// Shallower than the strip depth: nothing of it remains, so it matches nothing. The old
		// rule returned true here, and that is precisely the tolerance being removed.
		{"Dockerfile", "Dockerfile", 1, false},
		{"other", "Dockerfile", 1, false},
	}
	for _, tc := range cases {
		if got := matchesContextPath(tc.entry, tc.want, tc.strip); got != tc.match {
			t.Errorf("matchesContextPath(%q, %q, %d) = %v, want %v",
				tc.entry, tc.want, tc.strip, got, tc.match)
		}
	}
}

// TestFetchDockerfileFeedsTheFromCheck — the two halves of the guarantee together: the file is read
// out of a real artifact, and an unpinned FROM in it is refused.
func TestFetchDockerfileFeedsTheFromCheck(t *testing.T) {
	srv := contextServer(t, map[string]string{
		"app-abc/Dockerfile": "FROM golang:1.26\nRUN go build\n",
	})

	body, err := FetchDockerfile(context.Background(), srv.Client(), srv.URL, "", "Dockerfile", 1)
	if err != nil {
		t.Fatalf("FetchDockerfile: %v", err)
	}
	if err := CheckPinnedBases(bytes.NewReader(body)); err == nil {
		t.Fatal("an unpinned FROM in a real context was accepted")
	}
}
