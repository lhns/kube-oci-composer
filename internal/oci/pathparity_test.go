package oci

import (
	"archive/tar"
	"bytes"
	"testing"

	"github.com/lhns/kube-oci-composer/internal/archive/archivetest"
)

// TestTheComposerPlacesEntriesLikeTheBuilder is the guard ADR 0023 asked for and did not get.
//
// The composer's assembler and the builder's fetcher have different SINKS -- layer entries in
// memory versus files on a disk -- but they must agree exactly on WHERE an entry lands. A second
// copy of that rule was written for the builder, the two drifted, and every ImageBuild with a
// sourceRef context broke while the composer was fine (ADR 0045).
//
// Both now run the same table. A case either side gets wrong fails here.
func TestTheComposerPlacesEntriesLikeTheBuilder(t *testing.T) {
	for _, tc := range archivetest.PathCases() {
		t.Run(tc.Name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			body := []byte("x")
			hdr := &tar.Header{Name: tc.Entry, Mode: 0o644, Size: int64(len(body))}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}

			entries, err := extractTar(tar.NewReader(&buf), "", tc.Subpath, tc.Strip)

			// A subpath that selects nothing is refused rather than yielding an empty layer, and a
			// strip that consumes everything likewise. Both are the correct outcome for a case whose
			// entry contributes nothing, so the assertion is on the placement, not on success.
			if tc.Dest == "" {
				if err == nil && len(entries) != 0 {
					t.Errorf("entry %q should contribute nothing, got %d entries", tc.Entry, len(entries))
				}
				return
			}
			if err != nil {
				t.Fatalf("extractTar: %v", err)
			}

			var found bool
			for _, e := range entries {
				if e.name == tc.Dest {
					found = true
				}
			}
			if !found {
				t.Errorf("entry %q (strip %d, subpath %q) did not land at %q; got %v",
					tc.Entry, tc.Strip, tc.Subpath, tc.Dest, names(entries))
			}
		})
	}
}

// TestStripComponentsWorksForEveryFormat is the claim that this is a PATH rule rather than a tar
// flag: it lives in the collector, which every format routes through, so zip and deb get it for
// free. A per-format implementation is exactly the shape that broke.
func TestStripComponentsWorksForEveryFormat(t *testing.T) {
	t.Run("zip", func(t *testing.T) {
		f := openZip(t, []zipEntry{
			{name: "app-1.2.3/", mode: 0o755 | 0o20000000000},
			{name: "app-1.2.3/Dockerfile", body: "FROM scratch\n"},
		})
		entries, err := extractZip(f, "", "", 1)
		if err != nil {
			t.Fatalf("extractZip: %v", err)
		}
		if _, ok := byName(entries)["Dockerfile"]; !ok {
			t.Errorf("stripComponents did not apply to a zip; got %v", names(entries))
		}
	})

	t.Run("deb", func(t *testing.T) {
		deb := buildDeb(t, ".gz", []tarFile{
			{name: "./usr/", dir: true},
			{name: "./usr/bin/", dir: true},
			{name: "./usr/bin/tool", body: "#!/bin/sh\n"},
		})
		entries, err := extractDeb(bytes.NewReader(deb), "", "", 1)
		if err != nil {
			t.Fatalf("extractDeb: %v", err)
		}
		if _, ok := byName(entries)["bin/tool"]; !ok {
			t.Errorf("stripComponents did not apply to a deb; got %v", names(entries))
		}
	})
}

func names(entries []tarEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.name)
	}
	return out
}
