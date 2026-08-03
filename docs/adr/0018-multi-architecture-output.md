# 0018. Multi-architecture output

## Status

**Open.** Not implemented, and deliberately absent from the API — a declared field that stalls is
worse than a missing one, which is a mistake this project has already made once with `image:`.

## Context

Every artifact this controller produces is single-platform. A base image must name a
platform-specific manifest, and [ADR 0015](0015-base-images-are-platform-pinned.md) refuses a
multi-architecture index outright, because resolving one would mean the *controller* picking a
platform — so the output would stop being a function of the spec.

That refusal is right for the single-platform case and is exactly what blocks the multi-platform
one. The resolution is not to relax it but to move the choice into the spec: with an explicit
`platforms` list, selecting a child manifest per platform is spec-driven, and an index base
becomes correct rather than forbidden.

Nothing in the estate that motivated this project needs it today — the clusters are amd64. The
question is whether to build it before someone hits it, and what it costs.

## Options

**A. `spec.platforms`, one composition per platform, publish an index.**

```yaml
spec:
  base:
    ref: quay.io/strimzi/kafka:…@sha256:…   # an INDEX here, not a child
  platforms: [linux/amd64, linux/arm64]
```

Assembly runs once per platform, each resolving its own child of the base index, and the results
are published as an image index. Reasonably mechanical, but it touches more than it looks:

- **The input hash** must cover the platform list, and each child needs its own identity.
- **`status.artifact`** currently names one digest. With an index there is the index digest plus
  one per platform, and the thing a workload pins is the index.
- **`status.history` and garbage collection** must account for child manifests, or GC will
  reclaim the children of a retained index and leave a manifest pointing at nothing.
- **Content tags** — one per index, or one per child as well?
- **Platform inheritance** from the base config (ADR 0015) becomes per-child rather than global.

**B. Leave it single-platform, permanently.** Defensible if the answer to "who needs this" stays
"nobody". Multi-arch consumers can run one ImageComposition per platform and assemble the index
themselves, which is ugly but possible.

**C. Support it only for content that has no base.** A scratch bundle of files is platform-neutral
in practice; the platform only really enters through the base image. This is a much smaller change
— publish the same layers under an index naming several platforms — and covers the mounted-bundle
case without touching base resolution. It is also arguably dishonest, since the artifact would
claim platforms it was never built for.

## What would decide it

- **A real arm64 node in a cluster running this.** Until then the requirement is hypothetical and
  the design would be guesswork.
- **Whether the bundle case (C) is the one people actually hit.** If artifacts are mostly mounted
  rather than run, platform may be irrelevant to them, and C is nearly free. If they are mostly
  runnable images over a base, only A helps.

## Consequences of leaving it open

ADR 0015's refusal of index bases is stated as though permanent. It is not — it is correct only
while the platform list does not exist. That should be re-read as conditional if this is ever
picked up.
