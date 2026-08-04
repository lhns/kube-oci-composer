# 0017. How does a workload's reference get updated?

## Status

**Accepted.** Resolved by option D below: the consumer chooses the tag by hashing the build spec.
Amends [0010](0010-workloads-reference-digests.md), which said a workload must never reference a
tag.

## Context

[ADR 0010](0010-workloads-reference-digests.md) settles what a workload *references*: a digest,
never a tag. It does not settle how that digest gets into git, and the assumption it made —
"Flux image-reflector and image-automation close the loop" — turned out not to hold for the
cluster this was built for.

Two things came to light:

1. **Flux image automation is frequently not installed.** A stock `flux install` ships source,
   kustomize, helm and notification. The image-reflector and image-automation controllers are
   opt-in, and the estate this was written for does not run them. Building on their presence is
   building on an assumption.
2. **The inputs are as much of a problem as the output.** Renovate and similar tools discover
   images by recognising known structures — pod specs, Flux kinds. Nothing recognises
   `spec.base.image` inside a CRD it has never heard of. So a naive setup leaves the user
   hand-tracking upstream base image releases *as well as* hand-updating the output pin.

## Decision

**The consumer chooses the tag, by hashing the build.**

Whatever templates the `ImageComposition` — Helm here, but any templating tool works — hashes the
build-determining part of the spec, writes the result into `spec.publish.tags`, and uses the same
value in the consuming workload's image reference. One render, one value, two places.

```
{{- $spec := include "kafka.icspec" . }}      {{/* base + layers + config ONLY */}}
{{- $tag  := printf "s%s" (sha256sum $spec | trunc 16) }}

# ImageComposition   publish: {name: kafka-tiered-storage, tags: [{{ $tag }}]}
# Deployment         image:   registry/kafka-tiered-storage:{{ $tag }}
```

Nothing observes anything at runtime: no extra controllers, no git write-back, no status reading,
no scan interval, no lag. A worked example is in
[`docs/examples/spec-hash-tag`](../examples/spec-hash-tag/README.md).

**Why this is sound rather than a trick.** [ADR 0016](0016-the-scope-line-is-determinism.md) draws
the scope line at determinism: the output digest is a pure function of digest-pinned inputs. A hash
of those inputs therefore identifies the output as precisely as the output's own digest does. The
project's central guarantee is what makes this work — it would be unsafe in a tool that could `RUN`
things, which is exactly why this one cannot.

**The circularity dissolves.** The obvious objection is that the tag lives in the spec, so hashing
the spec would include it. But the tag is not an *input to the build*: where an artifact is
published does not change what it is. Hash a partial that excludes `publish`, and the problem
disappears without needing a ConfigMap or a `tagFrom` indirection.

### What this required

- `publish.tags` / `push.tags` became **lists**, optional, with no default. One build can carry a
  spec-hash tag and a readable pointer, or the same hash under several algorithms. Empty publishes
  by digest alone.
- **`immutable` defaults to true** on both, and is what makes referencing a tag safe: the tag will
  not be moved to different content, the build fails instead. Republishing identical content is
  always a no-op, so a steady reconcile loop never trips it.
- **The auto-generated `<tag>-<digest[:12]>` content tag was removed.** It existed only because
  `publish.tag` was a moving pointer and nothing else gave an immutable handle. A spec-hash tag
  *is* a content tag, and the digest is always addressable.

## Consequences

**Renovate still matters, for the inputs.** This decision settles the *output* reference only.
Point 2 above is unaddressed: `fetch.url` + `fetch.digest` and `base.image` + `base.digest` still
need a regex `customManager` for Renovate to track upstream releases. Merging `base.image` and
`base.digest` into one conventional `ref: repo:tag@sha256:…` remains worth doing and is not done.

**It only holds for inputs pinned in the spec.** `sourceRef` resolves a Flux revision at reconcile
time and `configMap` reads live content, so both can change while the spec, and therefore the tag,
does not. With `immutable: true` that surfaces as a terminal error rather than silent divergence —
the right failure, but one to expect rather than discover. Fold such inputs into the hash where
possible, or use `immutable: false` and reference the digest.

**Rollback is bounded by retention.** Rolling back a commit names an earlier spec-hash tag, which
resolves only while that build is retained (`publish.history`, ADR 0011).

**A pod may briefly fail to pull on first apply**, because the composition and the workload are
applied together while the build is still running.

## Alternatives, still valid in their situations

**A. Renovate against the output.** Works when the serving endpoint is reachable from CI. Requires
exposing it, which some users will not want. Still needed for the *inputs* regardless.

**B. Flux image-reflector plus image-automation.** Runs in-cluster, so nothing needs exposing.
Costs two controllers, three CRDs and git write credentials for Flux, and remains **untested**
against our tag layout. The right answer for anyone who cannot template the composition and the
workload together — a hand-written manifest, or a CR owned by a different chart.

**C. Hand-pinning a digest.** Works today with nothing installed. Fine at a few updates a year.

**E. The controller hashes the CR itself.** Considered and rejected: the controller sees the object
after API-server defaulting and managed-field rewriting, and could not guarantee a consumer
reproduces the same bytes from a different serialisation. Making the consumer the source of the
value avoids having to define a canonical CR encoding that both sides must agree on forever.
