# 0018. Multi-architecture output

## Status

**Accepted**, implemented as option A: `spec.platforms`, one assembly per platform, published as an
OCI image index.

Previously Open, and deliberately without an API field — "a declared field that stalls is worse
than a missing one". What decided it was not a new argument but a requirement: multi-architecture
output is wanted, and the question became how to default it rather than whether to build it.

## Context

Every artifact this controller produced was single-platform. A base image had to name a
platform-specific manifest, and [ADR 0015](0015-base-images-are-platform-pinned.md) refused a
multi-architecture index outright, because resolving one would mean the *controller* picking a
platform — so the output would stop being a function of the spec.

That refusal is right for the single-platform case and is exactly what blocked the multi-platform
one. The resolution was not to relax it but to move the choice into the spec.

## Decision

```yaml
spec:
  base:
    image: quay.io/strimzi/kafka
    digest: sha256:…        # an INDEX is now allowed here, when platforms names >1
  platforms: [linux/amd64, linux/arm64]
```

- **Two or more platforms** → one child image per platform, published as an OCI image index.
- **Exactly one** → a single image manifest, not an index wrapping one child. One platform is one
  image; wrapping would change the digest of every artifact that later adopts an explicit platform,
  for nothing.
- **Unset** → the base's platform if there is a base, otherwise the **controller's own
  architecture** (with the OS always `linux`).

### The default is the interesting part

Requiring `platforms` on every base-less artifact would be ceremony for the common case — a bundle
of platform-neutral files that is only ever mounted. So unset resolves to something concrete, and
for a base-less artifact that something is an environment value.

This is the only input to the output digest that does not come from the spec, and
[ADR 0002](0002-content-addressed-inputs.md) now records it as a stated exception. The short
version: on a single-architecture cluster it equals the value that was previously hardcoded, so
nothing churns; on an arm64 cluster it starts being correct; on a **mixed** cluster the same spec
can produce different content depending on where the leader runs, and the answer is to name
`platforms` or pin the controller with a `nodeSelector`.

### The layers are built once

Composed content is the same bytes on every platform — only the config differs. `AssembleIndex`
builds the layer tarballs once and shares them across children. That is not only an optimisation:
rebuilding per platform would spend real time producing identical layers, and any non-determinism
in that path would surface as children disagreeing about content they are supposed to share.
`TestAssembleIndexSharesLayers` pins it.

### What it touched, and why each mattered

The original "Options" section listed five consequences of option A. All five were real:

- **The input hash** now covers the resolved platform list *and* the base digest. The base digest
  turned out to be a pre-existing bug — it reached the output but not the hash, so repointing
  `spec.base.digest` short-circuited as unchanged and the new base was never built.
- **`status.artifact`** was deliberately left alone. What a workload pins is the index digest,
  which `Digest` already holds, and the type's comment says its shape is identical across every
  kind in the group. Per-platform detail belongs in history, not in what consumers read.
- **`status.history`** gained `BuildRecord.Manifests`, the child digests.
- **Garbage collection** reads that field. Marking is derived from status and never parses manifest
  bytes, so without it an index is retained while its children are swept — leaving a manifest that
  resolves to nothing, failing at pull time rather than at collection time, on a reference status
  still reports as published. `TestIndexChildManifestsSurvive` fails without the fix.
- **Persistence and replay** save and restore children *before* the index. An index restored alone
  resolves, passes a HEAD, and 404s every pull that follows its descriptors — the worst available
  outcome, because it looks healthy.

**`AssemblyVersion` was NOT bumped.** Its contract is "bump when the output changes for identical
inputs", and for the unset case the output does not change. Bumping it is the difference between
one cheap rebuild per object and rebuilding everything, everywhere, for no change in output.

## Alternatives rejected

**B. Leave it single-platform permanently.** Defensible only while the answer to "who needs this"
stays "nobody"; it stopped being.

**C. Index only for base-less content.** Much smaller — publish the same layers under an index
naming several platforms — and it would have covered every artifact in the estate today, all of
which are base-less bundles. Rejected because it is dishonest in the general case: the artifact
would claim platforms it was never built for, and the moment someone adds a base it stops working
without warning. Option A subsumes it: a base-less composition with two platforms produces exactly
what C would, having actually assembled both.

## Consequences

Naming `platforms` changes the spec, so the spec-hash tag downstream changes with it and opting in
gets a fresh tag rather than colliding with an immutable one. Adoption is therefore incremental and
per-artifact.

An arm64 node in a cluster running this remains the only thing that would exercise the index path
end to end against a real kubelet. The e2e test covers the shape; it cannot cover the selection.
