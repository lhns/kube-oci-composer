# 51. A build that lost its image rebuilds it

## Status

Accepted. **Reverses part of [ADR 0025](0025-dockerfile-builds-as-a-second-kind.md)**, which assumed
storage durability for `ImageBuild` instead of verifying it.

## Context

`github-runner/github-runner` had lost every tag it had. The manifest survived only as an untagged
blob — what `deleteUntagged` reclaims next — and its `ImageBuild` reported `Ready=True` throughout.

[ADR 0048](0048-retention-addresses-the-registry-it-can-reach.md) explains how the images stopped
being protected. This one is about the second half: **nothing noticed.** Three days, hourly
reconciles, and the object's own view of itself never changed.

The reason was explicit and deliberate:

> Note what is NOT checked here — that the artifact is still present in the registry. The composer
> verifies that with one HEAD because it can rebuild identical bytes if it is gone; a rebuild here
> might not produce the same digest, so re-verifying would risk turning a missing artifact into a
> permanent immutable-tag conflict.

Every clause of that is true. `ImageComposition` is `output = f(spec)`, so `ResolvePublished` plus
`published.Matches(prev.Digest)` lets it re-publish **identical bytes** and self-heal — it always
has. `ImageBuild` is not reproducible, so a rebuild produces different content under the same name.

The conclusion drawn from it was that the safer thing was not to look. Three days of silent,
ongoing loss is the evidence against that.

## Decision

**`ImageBuild` verifies that what it published is still there, and rebuilds when it is not.**

The short-circuit gains a conjunct, the way the composer's already had one. Falling through *is* the
trigger: the next lines are `currentJob`, `checkTagConflict` and `startBuild`, so no new path
starts a build.

**Both the digest and every tag are checked.** A lost tag is not cosmetic — the surviving manifest
is untagged, which is exactly what the shipped policy reclaims next. Checking only the digest would
have called `github-runner` healthy.

**The tags come from the spec, never from `status.artifact.tags`.** Those are stored through the
public host, which is the trap ADR 0048 was written about, sixty lines away in the same codebase.

**Only a definite 404 is a loss.** Every other outcome — unreachable, unauthorised, timed out,
rate-limited — answers "present". Otherwise one registry outage starts a build for every
`ImageBuild` in the cluster simultaneously, which is the worst available response to a registry that
is already struggling, and self-sustaining once those builds begin pushing. **Fail towards doing
nothing.**

**It is loud.** A Warning, `ArtifactLost`, saying in as many words that the new image will have a
different digest.

### What this costs, stated plainly

**The rebuild replaces; it does not restore.** Anything pinned to the old digest — a Deployment
referencing by digest, a spec-hash tag — is not helped. Those bytes are gone and no decision here
brings them back.

**`onConflict: immutable` will not stop it, because there is nothing left to conflict with.**
`checkTagConflict` asks whether the tag already holds something; the tag is gone, so it holds
nothing, and the push proceeds. A tag that was immutable now carries different content. That is a
real weakening of that guarantee, and it is the price of the object recovering by itself. The Event
is what keeps it from being silent.

**The loop is bounded for free.** The check runs once per reconcile and reconciles are
interval-paced, so a registry actively deleting content costs at most one build per `spec.interval`.
No backoff state was needed.

**One HEAD per tag plus one per digest, per reconcile.** The composer has always paid this.

## Consequences

**ADR 0025's durability assumption is now enforced rather than assumed.** It said storage durability
"stops being optional" for this kind. It still does — this recovers a name, not the bytes — but the
object now notices, which is the difference between a degraded system and a silent one.

**An `ImageBuild` can change what a tag points at without its spec changing.** Previously only a
spec change could do that. The trigger is external deletion, and it is announced.

**Restoring a truly immutable `ImageBuild` is still not possible**, and cannot be, without storing
the image somewhere this project does not. Anyone who needs that guarantee needs registry-side
durability — replication or backups — and the docs continue to say so.
