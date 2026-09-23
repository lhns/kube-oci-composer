package reconciler

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// DigestTag is the tag naming a manifest after its own digest: "digest-<hex>".
//
// Both kinds apply it to everything they publish, beside the spec's tags (ADR 0060), because of two
// behaviours of the bundled registry, zot:
//
//   - Moving a manifest's LAST tag drops it from the repository index, deleting the previous build
//     under anything pinned to it (zot#4444). A second name keeps it.
//   - A manifest that loses its last tag loses its retention statistics and is never reclaimed;
//     always carrying a tag is what lets the chart drop keepUntagged.
//
// Derived from the content, so it can never be remeaned or conflict.
//
// The name must never contain "sha256-": that is the OCI referrers tag schema, which zot matches
// unanchored and then never evaluates for retention (keeping the artifact forever), and which a
// referrers fallback on other registries would misread. Only the hex: every digest here is sha256.
func DigestTag(digest string) string {
	_, hex, found := strings.Cut(digest, ":")
	if !found {
		hex = digest
	}
	return "digest-" + hex
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

// HasDigestTag reports whether a list of tags from status includes the digest's own tag, qualified
// ("host/repo:tag", as in status.artifact) or bare (as in a composition's history). An object
// published before the tag existed does not claim it, which is how the backfill finds it.
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

// ApplyDigestTagWithReferrers is ApplyDigestTag for digest and then for every referrer of it: the
// SBOM and provenance the composer attaches (ADR 0008), which were pushed untagged before ADR 0060.
// For registries that expire untagged content by age; zot keeps a referrer while its subject
// exists regardless.
//
// Any failure is returned, so the caller does not record the subject as tagged and the next pass
// retries the lot. ApplyDigestTag is idempotent, so repeating the subject costs one request.
func ApplyDigestTagWithReferrers(repo, digest string, refOpts []name.Option, opts []remote.Option) error {
	if err := ApplyDigestTag(repo, digest, refOpts, opts); err != nil {
		return err
	}
	subject, err := name.NewDigest(repo+"@"+digest, refOpts...)
	if err != nil {
		return Terminal("invalid reference %s@%s: %v", repo, digest, err)
	}
	idx, err := remote.Referrers(subject, opts...)
	if err != nil {
		return fmt.Errorf("listing referrers of %s: %w", digest, err)
	}
	mf, err := idx.IndexManifest()
	if err != nil {
		return fmt.Errorf("reading referrers of %s: %w", digest, err)
	}
	for _, d := range mf.Manifests {
		if err := ApplyDigestTag(repo, d.Digest.String(), refOpts, opts); err != nil && !IsNotFound(err) {
			return err
		}
	}
	return nil
}

// NeedsDigestTag reports whether a history entry is still waiting for its digest's own tag: it
// names a digest, does not claim the tag, and has not been found lost.
func NeedsDigestTag(rec ociv1alpha1.BuildRecord) bool {
	return rec.Digest != "" && rec.Lost == nil && !HasDigestTag(rec.Tags, rec.Digest)
}

// DigestTagOutcomes records, per digest, what one backfill pass learned: tagged, or found gone.
// A digest that failed any other way is absent and is retried next pass.
type DigestTagOutcomes map[string]bool

// Record notes the result of applying a digest's own tag.
func (o DigestTagOutcomes) Record(digest string, err error) {
	switch {
	case err == nil:
		o[digest] = true
	case IsNotFound(err):
		o[digest] = false
	}
}

// Tagged reports whether digest was given its own tag on this pass.
func (o DigestTagOutcomes) Tagged(digest string) bool { return o[digest] }

// Changed reports whether this pass learned anything status should record.
func (o DigestTagOutcomes) Changed() bool { return len(o) > 0 }

// MarkHistory records the pass on history: a tagged entry gains its tag (as tagFor renders it, which
// differs by kind), and an entry the registry no longer serves is marked Lost -- unless it is the
// current artifact, whose loss is its controller's to repair, not a record to retire.
func (o DigestTagOutcomes) MarkHistory(history []ociv1alpha1.BuildRecord, current *ociv1alpha1.ArtifactStatus,
	tagFor func(string) string, now metav1.Time) {
	for i := range history {
		rec := &history[i]
		tagged, known := o[rec.Digest]
		switch {
		case !known || !NeedsDigestTag(*rec):
		case tagged:
			rec.Tags = append(rec.Tags, tagFor(rec.Digest))
		case current == nil || current.Digest != rec.Digest:
			rec.Lost = now.DeepCopy()
		}
	}
}
