# 60. Every manifest carries its own name

## Status

**Accepted.** Supersedes the part of [0017](0017-updating-the-consumed-digest.md) that removed the
auto-generated content tag. Amends [0010](0010-workloads-reference-digests.md),
[0011](0011-content-tags-expire.md), [0031](0031-the-retention-guarantee.md),
[0049](0049-a-reference-that-is-gone-is-a-fact-not-a-failure.md) and
[0054](0054-name-it-after-you-push-it.md).

## Context

Two behaviours of zot v2.1.21, both measured, both invisible to everything ADR 0031 relies on.

**Moving a tag deleted the digest it moved off.** zot drops a manifest from the repository index
when its *last* tag moves to another digest: `CheckIfIndexNeedsUpdate` splices out the descriptor
carrying the tag and does not re-add the old digest untagged. With `push.tags: [main]` and
`onConflict: Overwrite` — the configuration the CRD recommends for a moving tag — the previous build
stopped resolving about five seconds after the next push, while a Ready object still listed it in
`status.history` and the refresher was pulling it. Not expiry: zot's retention pass ran afterwards
and never evaluated the old digest. Reproduced through this controller (e2e run 35760893931). Filed
upstream as [zot#4444](https://github.com/project-zot/zot/issues/4444); the fix, PR #4447, is
unreleased. Distribution, Harbor and Quay keep the old digest addressable; zot is the outlier.

**Retired images never left the disk.** Removing a manifest's last tag deletes its retention
statistics (`RemoveRepoReference`). With `keepUntagged` configured, a statistics-less untagged
manifest is then retained without being evaluated (`"untagged manifest statistics not found"`), and
a pull cannot recreate the statistics. The manifest keeps referencing its layers, so they are never
freed either. Every tag this policy expired left its whole image behind: 252 blob sweeps reclaimed 9
blobs. Without `keepUntagged` zot falls back to reclaiming untagged manifests by age. The behaviour
arrived with the `keepUntagged` feature (zot PR #4191), is unreported upstream, and zot's docs
describe the opposite. PR #4447 would make it worse: it keeps a displaced digest as an untagged row,
which `keepUntagged` then pins.

## Decision

**Both kinds name everything they publish after its own digest, `digest-<hex>`,** beside whatever
the spec asks for. Digest-only publications and attestation referrers included.

- A rolling tag is then never a manifest's last tag, so moving it takes nothing with it.
- Everything live carries a tag that the refresher's pulls renew — a pull by digest renews the tag
  on that digest, measured — so nothing live depends on `keepUntagged`. The chart can drop it, and
  a retired manifest is then reclaimed `gcDelay` after its last tag expires, layers included.

**Not `sha256-<hex>`.** That was the first choice, and it made every artifact immortal. It is the
OCI referrers tag schema — where a client without the Referrers API keeps the referrers index *for*
subject `sha256:<hex>` — and zot treats any tag matching `sha256\-[A-Za-z0-9]*$`, unanchored at the
start, as one: `OnUpdateManifest` returns before recording it, so retention never has statistics
for it and keeps it (`"tag statistics not found"`). The e2e's digest-only control caught it,
surviving 600s. The name also collides with that schema on any registry: a referrers fallback would
read the manifest as an index, or overwrite the tag. So the name contains no `sha256-` anywhere, and
a unit test holds it to that.

Rules that make it safe:

- **Accepted publishes only.** Under `onConflict: Fail` or `Keep` nothing is named, so refused
  content stays untagged and reclaimable. Naming it would make it permanent.
- **Appended last.** `status.artifact.revision` and `.ref` are built from the spec's first tag and
  do not move.
- **Backfilled, never rebuilt.** Objects published before this gain the tag on their converged
  path, driven by status. Reading the missing tag as a loss would have rebuilt every `ImageBuild` on
  upgrade — to a different digest — and reassembled every composition.
- **Chart order.** `registry.retention.keepUntagged` ships on, and defaults off one release later.
  Off before objects are backfilled exposes a still-untagged digest-only publication or attestation
  to collection by age, while something pulls it.

Also: a gone `status.artifact` is no longer as quiet as expired history. The refresher raises
`ArtifactLost` for it and counts it towards the Degraded escalation. History stays quiet, as ADR
0049 intended; that record simply drew no line between the two.

## Consequences

- **Storage is bounded again on the bundled registry**, once `keepUntagged` is off. Before, it only
  grew.
- **Multi-platform images are covered whole.** Measured against zot v2.1.21 with `keepUntagged`
  off: while an index was refreshed the way the refresher does it — the index and its amd64 child —
  all 17 children and 34 blobs survived, including platforms and attestation manifests nothing
  pulled. Once refreshing stopped, all of them were reclaimed. zot keeps what a live index references.
- **One more tag per build**, in the registry and in `status.artifact.tags` / `status.history`. It
  appears in a tag listing, so an image-automation policy that picks from every tag has to exclude
  `^digest-`.
- **ADR 0031's guarantee now rests on "everything we publish carries a tag"**, not on pull recency
  applying to untagged content. That is load-bearing and tested per kind, plus a structural parity
  test that both kinds do it.
- **Content attached by hand** (`cosign attest`, `oras attach`) is untagged and loses its
  protection when `keepUntagged` goes. `keepUntagged: true` is the escape hatch, at the cost of the
  leak.
- **With `keepUntagged` off, `gcDelay` is the only cover** for a build's output between push and
  naming (ADR 0054). Losing that race costs a rebuild, not data: nothing references an output before
  it is named. The default is not raised for it.
- ADR 0031's note that "retagging or untagging frees nothing" was true of `keepUntagged`, not of
  zot: untagging frees space once it is off, and on zot a tag *move* drops the old manifest at once.

## Rejected

- **The controller deletes what it no longer needs.** Registries cannot list untagged manifests, so
  a sweep can only act on what the controller remembered, and `status.history` is destroyed by the
  eviction that creates the garbage. It would need delete credentials, reference counting across
  both kinds in two processes that cannot read each other (ADR 0004), and an always-on `ImageBuild`
  finalizer. And it turns ADR 0031 from "cannot delete" into "deletes correctly".
- **Only tag when the spec asks for tags.** It keeps `tags: []` reclaimable under `keepUntagged` —
  but that only matters while `keepUntagged` is configured, which is the thing being removed. One
  rule was preferred.
- **Wait for zot#4444.** Unreleased, and it does not touch the leak — it makes it worse.
- **Tell users to add a unique tag.** That is this decision, left to each user to remember.
