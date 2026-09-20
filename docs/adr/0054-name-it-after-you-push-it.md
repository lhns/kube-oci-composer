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

**A failed tagging step fails the reconcile after a successful build.** The content is pushed but
unnamed, so it is not published; retrying re-reads the same Job and re-applies the same names, which
is idempotent. Content orphaned by a permanent failure is reclaimed as untagged.

**Tests of the publish path now need a registry.** They previously used an invented digest, because
the controller never spoke to one on success. That is inherent to the change.

**A build that is refused still cost a build.** Under `Fail` the work is done and discarded. The
pre-flight check exists to make that rarer, not impossible — it cannot be, because the digest is
the thing being decided on and it does not exist until the build is over.
