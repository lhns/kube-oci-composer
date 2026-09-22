package reconciler

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// DigestTag is the tag naming a manifest after its own digest: "sha256-<hex>".
//
// Both kinds apply it to everything they publish, beside whatever the spec asks for. ADR 0060. It
// exists for two behaviours of the bundled registry, zot:
//
//   - Moving a manifest's LAST tag drops the manifest from the repository index, so a rolling tag
//     deleted the previous build under anything still pinned to it (zot#4444). A second name keeps it.
//   - A manifest that loses its last tag loses its retention statistics with it, and is then never
//     reclaimed. Content that always carries a tag while it is live is what lets the chart stop
//     configuring keepUntagged, which is what pins such manifests.
//
// Derived from the content, so it can never be remeaned and can never conflict. cosign's ".sig"
// tag is the same transformation with a suffix.
func DigestTag(digest string) string {
	return strings.Replace(digest, ":", "-", 1)
}

// PublishTags is tags with the digest's own tag appended.
//
// Appended, never prepended: tags[0] is what status.artifact.revision and .ref are built from, and
// adding a name must not change what either says.
func PublishTags(tags []string, digest string) []string {
	own := DigestTag(digest)
	out := make([]string, 0, len(tags)+1)
	for _, t := range tags {
		if t != own {
			out = append(out, t)
		}
	}
	return append(out, own)
}

// HasDigestTag reports whether a list of tags from status includes the digest's own tag. Qualified
// ("host/repo:tag") or bare, since status.artifact stores the one and a composition's history the
// other.
//
// Status is the record of what was applied: an object published before the tag existed does not
// claim it, and that is how the backfill finds it.
func HasDigestTag(tags []string, digest string) bool {
	own := DigestTag(digest)
	for _, t := range tags {
		if t == own || strings.HasSuffix(t, ":"+own) {
			return true
		}
	}
	return false
}

// ApplyDigestTag names content that is already in repo after its own digest.
//
// A missing manifest comes back as the registry's 404, unwrapped, so IsNotFound tells a record
// that has already expired from a registry that did not answer.
func ApplyDigestTag(repo, digest string, refOpts []name.Option, opts []remote.Option) error {
	ref, err := name.NewDigest(repo+"@"+digest, refOpts...)
	if err != nil {
		return Terminal("invalid reference %s@%s: %v", repo, digest, err)
	}
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return err
	}
	tag, err := name.NewTag(repo+":"+DigestTag(digest), refOpts...)
	if err != nil {
		return Terminal("invalid tag for %s: %v", digest, err)
	}
	if err := remote.Tag(tag, desc, opts...); err != nil {
		return fmt.Errorf("tagging %s as %s: %w", digest, DigestTag(digest), err)
	}
	return nil
}
