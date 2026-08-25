# 43. An OCI artifact is a source we own

Date: 2026-08-25

## Status

**Accepted, not yet implemented.** Follows from the rules in
[0042](0042-content-addressed-not-flux.md).

Deciding ahead of the code, the way [0008](0008-supply-chain.md) decided the supply-chain design
long before [0040](0040-the-supply-chain-work-is-worth-building.md) built it. The point of writing
it now is that 0042's table would otherwise show a blank cell that reads as an oversight, and the
next person to notice it would either re-derive this argument or reach for `OCIRepository` and
assume that was the intent.

## Context

0042 says we resolve sources the spec addresses exactly and whose address verifies the bytes, and we
delegate sources that need a mutable ref tracked over time.

An **OCI artifact** — a blob pushed to a registry with `oras`, `flux push artifact` or equivalent,
carrying an arbitrary `artifactType` and usually one tar layer — passes both rules when it is pinned
by digest. The spec names it exactly. The manifest digest verifies what came back. There is nothing
to poll and no credential to hold for discovery.

We do not implement it. Today consuming one means a Flux `OCIRepository`, which is Flux doing work
Flux is not needed for.

**`image` does not already cover it.** `source.PullImage` contributes a *container image's* layers
as they are — deliberately, so they stay content-addressed and shared rather than being unpacked and
repacked — and it switches on `desc.MediaType`, refusing an index without a platform. An artifact
with an arbitrary `artifactType` and a single tar layer is a different shape being asked a different
question: not "put this image's filesystem here" but "unpack this blob here".

## Decision

**An OCI artifact pinned by digest is a source this project implements, for both kinds.**

Not built yet. When it is built, it applies to `ImageComposition` layers and to
`ImageBuild.spec.context` alike — the same verb in both vocabularies, since 0042's rules are about
the source and not about the kind consuming it.

**Deliberately left open: whether it extends `image` or becomes its own verb.** A distinct verb is
the likely answer — "contribute an image's layers" and "unpack an artifact's blob" are different
operations, and `image`'s doc comment is explicit about the first — but the choice wants a look at
what real artifacts in this project's path actually look like, and guessing now would bake an
assumption into an API that cannot easily shed it.

## Consequences

**Until it is built, `OCIRepository` is the workaround**, and it is a reasonable one. This record
exists so that using it is understood as a workaround rather than as the design.

**When it is built, it will be the first thing in this line of work to touch `ImageComposition`.**
Everything else in the current plan is `ImageBuild`-only. That is worth knowing for review: it is a
change to the kind with the stronger promise, so rule 2 has to hold exactly — the digest must be
verified against the bytes, not merely declared and trusted.

**It does not weaken 0042's git decision.** Artifacts are addressable and verifiable; git trees are
neither without implementing git. The two questions look similar and are not.
