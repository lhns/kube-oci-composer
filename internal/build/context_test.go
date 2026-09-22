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

// contextServer serves a gzipped tar of the given entries.
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

// TestFetchDockerfileStripsTheWrapperDirectory: with a strip depth of 1, a release tarball's
// unpredictable top-level directory is removed before matching.
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

// TestFetchDockerfileHonoursSubpathAndName: subpath and file name compose.
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

// TestFetchDockerfileMissing: a typo in spec.dockerfile is an error, not a skipped FROM check.
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

// TestFetchDockerfileRejectsBadStatus: a collected artifact's 404 is not an empty Dockerfile.
func TestFetchDockerfileRejectsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	if _, err := FetchDockerfile(context.Background(), srv.Client(), srv.URL, "", "Dockerfile", 1); err == nil {
		t.Fatal("a 404 was accepted")
	}
}

// TestMatchesContextPath pins the one path rule shared with the extractor, at both depths (ADR
// 0045).
func TestMatchesContextPath(t *testing.T) {
	cases := []struct {
		entry, want string
		strip       int
		match       bool
	}{
		// Nothing stripped, as for a Flux artifact.
		{"Dockerfile", "Dockerfile", 0, true},
		{"./Dockerfile", "Dockerfile", 0, true},
		{"services/api/Dockerfile", "services/api/Dockerfile", 0, true},
		{"app-abc/Dockerfile", "Dockerfile", 0, false},

		// stripComponents: 1, as for a release tarball.
		{"app-abc/Dockerfile", "Dockerfile", 1, true},
		{"./app-abc/Dockerfile", "Dockerfile", 1, true},
		{"app-abc/services/api/Dockerfile", "services/api/Dockerfile", 1, true},
		{"app-abc/nested/Dockerfile", "Dockerfile", 1, false},
		{"app-abc/Dockerfile.dev", "Dockerfile", 1, false},

		// Shallower than the strip depth: nothing of it remains.
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

// TestFetchDockerfileFeedsTheFromCheck: a file read out of a real artifact is refused for an
// unpinned FROM.
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
