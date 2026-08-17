package v1alpha1

import "testing"

// TestBaseImageRepositoryAcceptsBothSpellings — `ref` and `image`+`digest` name the same base, and
// the accessor is what makes them interchangeable. If they ever disagreed, the input hash would
// move when a spec was rewritten from one spelling to the other, rebuilding and republishing every
// artifact for no change in content.
func TestBaseImageRepositoryAcceptsBothSpellings(t *testing.T) {
	const digest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	split := BaseImage{Image: "quay.io/strimzi/kafka", Digest: digest}
	combined := BaseImage{Ref: "quay.io/strimzi/kafka:0.43.0@" + digest}

	sRepo, sDigest := split.Repository()
	cRepo, cDigest := combined.Repository()

	if sRepo != cRepo || sDigest != cDigest {
		t.Errorf("spellings disagree: split gave (%q, %q), ref gave (%q, %q)",
			sRepo, sDigest, cRepo, cDigest)
	}
	if sDigest != digest {
		t.Errorf("digest = %q, want %q", sDigest, digest)
	}
}

// TestSplitPinnedRef — the registry-port case is the one that bites. "registry:5000/repo@sha256:…"
// has a colon that is not a tag separator, and stripping at the last colon regardless would turn
// the reference into "registry", which resolves to something else entirely.
func TestSplitPinnedRef(t *testing.T) {
	const digest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := map[string]struct{ ref, wantRepo string }{
		"tagged":            {"ghcr.io/lhns/app:v1.2.3@" + digest, "ghcr.io/lhns/app"},
		"untagged":          {"ghcr.io/lhns/app@" + digest, "ghcr.io/lhns/app"},
		"registry port":     {"registry:5000/lhns/app@" + digest, "registry:5000/lhns/app"},
		"port and tag":      {"registry:5000/lhns/app:v1@" + digest, "registry:5000/lhns/app"},
		"single segment":    {"busybox@" + digest, "busybox"},
		"single with tag":   {"busybox:latest@" + digest, "busybox"},
		"deep path":         {"ghcr.io/a/b/c/d:v1@" + digest, "ghcr.io/a/b/c/d"},
		"digest-like tag":   {"ghcr.io/lhns/app:sha256-abc@" + digest, "ghcr.io/lhns/app"},
		"underscore in tag": {"ghcr.io/lhns/app:v1_2@" + digest, "ghcr.io/lhns/app"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo, gotDigest := splitPinnedRef(tc.ref)
			if repo != tc.wantRepo {
				t.Errorf("repository = %q, want %q", repo, tc.wantRepo)
			}
			if gotDigest != digest {
				t.Errorf("digest = %q, want %q", gotDigest, digest)
			}
		})
	}
}

// TestImageSourceRepository — the same parsing, reached through the layer verb.
func TestImageSourceRepository(t *testing.T) {
	const digest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	repo, got := (&ImageSource{Ref: "registry:5000/team/tool:v2@" + digest}).Repository()
	if repo != "registry:5000/team/tool" || got != digest {
		t.Errorf("got (%q, %q)", repo, got)
	}
}
