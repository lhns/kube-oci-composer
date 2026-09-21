# 54. Name it after you push it

## Status

Accepted. Makes true what [ADR 0029](0029-three-valued-tag-conflict-policy.md) already claimed,
and removes the asymmetry it recorded as unavoidable.

## Context

ADR 0029 says the conflict policy is *"identical on both kinds, deliberately. An operator moving
between them should not have to learn a second set of semantics for the same question."* The CRD
field says it *"decides what happens when a tag already resolves to different content"*.

`ImageComposition` delivers that. It builds, then asks
`published.Conflicts(tags, digest)` with the digest it has just produced: a tag holding anything
else is a conflict, a tag holding the same digest is not.

`ImageBuild` could not. buildctl pushed **and named** in one operation, so the controller never saw
the new digest before it was published. The check therefore ran *before* the build and substituted
`status.artifact.digest` — the object's **previous** digest — for the one it could not know.

### The substitution had a hole, and only one

`Conflicts` reports a tag holding anything other than the digest passed in. With the previous
digest passed:

| tag holds | outcome | correct? |
|---|---|---|
| another object's digest | conflict | yes |
| nothing | no conflict | yes |
| **this object's own previous digest** | **no conflict** | **no** |

The last row is the whole defect. A tag holding this object's own previous digest was exempt, so
**an object remeaning its own tag was never a conflict** — and that is the ordinary case, because a
tag almost always holds what this object put there last time. On `ImageBuild`, `onConflict: Fail`
was close to inert.

Normally harmless: when the spec changes, a spec-hash tag changes with it, so the build writes a
*fresh* tag and there is nothing to conflict with. It bites when the tag fails to move while the
inputs do — reported from a live cluster, where a consuming repo rendered the tag from one Flux
source and the manifests from another and delivered a new Dockerfile under a stale tag. Setting
`onConflict: Fail` could not have caught it, because it was set.

## Decision

**The Job uploads the content. The controller names it.**

`--output type=image,…,push-by-digest=true` with no tag names. The build pushes a manifest
addressed only by its digest. Then `applyTags` resolves what the tags currently hold, calls
`published.Conflicts(tags, digest)` with the **real** digest, and applies the policy:

- **Fail** — refuse terminally, tag nothing. The pushed manifest is untagged, and the registry's
  `deleteUntagged` reclaims it.
- **Keep** — tag nothing, and record `status.conflict` with a **real `Dropped` digest**. ADR 0029
  documented that field being empty on this kind as unavoidable *"because no build is run at all"*.
  Uploading before naming is what makes it available.
- **Overwrite** — tag.

Tagging is `remote.Get` for the descriptor and `remote.Tag`, with the credentials the controller
already holds. A controller applying a tag is an internal process, consistent with the rule that
only internal processes write to this registry.

**The pre-build `checkTagConflict` stays, as an optimisation.** It can still notice that a tag holds
something foreign and decline to burn a build pod. It remains approximate, and that is now harmless:
it is advisory, and `applyTags` is authoritative.

## Consequences

**A tag meant to move now conflicts under `Fail`.** `latest` alongside a spec-hash tag, under the
default policy, will stall on every change — because that genuinely is changing what `latest`
means, and `Fail` is the setting that refuses to. `onConflict: Overwrite` is the answer and needs no
new API. **This is breaking** for anyone relying on the old exemption.

**No per-tag policy.** Mixing one moving tag and one immutable tag on a single object would need
one, and it is a configuration to avoid rather than to build for: a spec-hash tag never collides
with itself, so `Overwrite` costs it nothing.

**The manifest is UNTAGGED until this controller names it, and untagged is what a collector
reclaims.** That is the cost of the separation, and it was underestimated when this was written.

zot deletes untagged manifests by default -- including in a repository matching no retention
policy, where `HasDeleteUntagged` returns true precisely because none was found -- and `keepUntagged`
cannot save a manifest that was just pushed and never pulled. When the untagged manifest is the
repository's only content, `cleanRepo` removes the repository with it, so the read-back fails
`NAME_UNKNOWN` rather than reporting a missing manifest.

The window is push to `applyTags`, bounded by the controller's 15s Job poll. `gcDelay` is the
margin: the shipped 1h gives 240x, and the e2e's 1s gave none, which is where this was found --
intermittently, because zot walks repositories on a rotation, so a green run proved nothing.

The chart therefore refuses a `gcDelay` under 10m while `deleteUntagged` is true, and offers
`registry.retention.deleteUntagged: false` for a configuration that wants a fast collector without
the race. `keepUntagged` gained `pushedWithin`, which it should have had: pull recency alone
protects nothing that has only ever been pushed.

**A read-back that fails is PENDING, not a failure.** Nothing about this object's spec would fix
it, so ADR 0009's third path applies: a short fixed requeue rather than exponential backoff. But
retrying only helps if the registry is catching up; if a collector took the manifest, the content
is gone and no number of retries returns it. The message says both, and points at `gcDelay`.

**A failed tagging step fails the reconcile after a successful build.** The content is pushed but
unnamed, so it is not published; retrying re-reads the same Job and re-applies the same names, which
is idempotent. Content orphaned by a permanent failure is reclaimed as untagged.

**Tests of the publish path now need a registry.** They previously used an invented digest, because
the controller never spoke to one on success. That is inherent to the change.

**A build that is refused still cost a build.** Under `Fail` the work is done and discarded. The
pre-flight check exists to make that rarer, not impossible — it cannot be, because the digest is
the thing being decided on and it does not exist until the build is over.
