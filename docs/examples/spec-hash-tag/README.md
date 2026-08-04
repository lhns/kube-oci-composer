# Spec-hash tags

How a workload references a composed artifact without image automation, without git write-back,
and without reading anything from status.

**The problem.** A Pod spec cannot read another object's status, so the reference to a composed
artifact is a literal string in git. Something has to put the right string there. Flux
image-automation can, but it is opt-in and frequently not installed. Renovate can, but it runs in
CI and has to reach the serving endpoint. Hand-pinning works and never stops being manual.

**The idea.** Let whatever templates the `ImageComposition` choose the tag, by hashing the build.
Both the composition and the workload then derive the same value from one source in one render, so
nothing has to observe anything at runtime.

```
        values.yaml + templates
                  │
      ┌───────────┴───────────┐
      │  "spec-hash-tag.build" │   base + layers + config
      └───────────┬───────────┘
                  │ sha256 | trunc 16
                  ▼
             sc63f2fb1bc59a097
                  │
      ┌───────────┴────────────┐
      ▼                        ▼
 publish.tags[0]          image reference
 (what is built)          (what is pulled)
```

**Why it is sound.** [ADR 0016](../../adr/0016-the-scope-line-is-determinism.md) draws this
project's scope line at determinism: the output digest is a pure function of digest-pinned inputs.
So a hash of those inputs identifies the output as precisely as the output's own digest does. This
would not be safe in a tool that could `RUN` things, and that is exactly why this one cannot.

## Reading the example

- `templates/_helpers.tpl` — `spec-hash-tag.build` holds the build-determining spec and **nothing
  else**; `spec-hash-tag.tag` hashes the rendered partial.
- `templates/imagecomposition.yaml` — renders the same partial and sets `publish.tags` from the
  hash.
- `templates/deployment.yaml` — references the identical value.
- `rendered.yaml` — the output, so the result is readable without running Helm.

Render it yourself:

```sh
helm template demo docs/examples/spec-hash-tag/
```

## The two things people get wrong

**1. Do not hash `.Values`.** Hash the *rendered* partial. A template change can alter the spec
without altering values — a new field, a changed default, a conditional — and hashing values would
miss it, republishing different content under an unchanged tag. That is the precise hazard
`immutable` exists to catch, so you would be relying on the guard instead of being correct.

**2. Do not put `publish` in the hashed partial.** Where an artifact goes is not part of what it
is, and `publish.tags` is derived from the hash, so including it would be circular. Verified in
the example: changing `repository` or `history` leaves the tag alone, while changing a layer's
digest moves it.

## Why `immutable` matters here

`publish.immutable` defaults to true and should stay that way. A spec-hash tag can only ever mean
one thing — change the spec and the tag changes with it — so if the controller ever finds that tag
resolving to different content, something is genuinely wrong: an unpinned input, or two specs
colliding. Failing loudly is right.

It is also what makes `pullPolicy: IfNotPresent` correct on a *tag*. A cached copy cannot be stale
if the tag identifies content. Turn immutability off and that stops being true.

Republishing identical content under the same tag is always a no-op, so a steady reconcile loop
never trips the guard. Only a real change of meaning does.

## When this does not work

**Inputs that are not pinned in the spec.** `sourceRef` resolves a Flux revision at reconcile time
and `configMap` reads live content — both can change while the spec, and therefore the tag, does
not. With `immutable: true` that surfaces as a terminal error rather than silent divergence, which
is the right failure but should be expected rather than discovered. Options:

- fold the input into the hash where you can — a ConfigMap rendered by the same chart is easy;
- or set `immutable: false` and treat the tag as a pointer, referencing the digest instead;
- or pin the input properly with `fetch` + `digest`.

**A rollback older than `publish.history`.** Rolling back a commit names an earlier spec-hash tag,
which only resolves while that build is retained. Raise `history` if you roll back often; layers
are shared between builds, so it costs much less than the count suggests.

**The first apply.** Helm applies the `ImageComposition` and the workload together, so the pod may
briefly `ImagePullBackOff` while the build runs. It resolves itself.

## Alternatives

Still valid, and better in some situations — see [ADR 0017](../../adr/0017-updating-the-consumed-digest.md):

| | when it fits |
|---|---|
| **Flux image-automation** | the CR and the workload are not templated together, and you can run two more controllers |
| **Renovate** | already in use, and the serving endpoint is reachable from CI |
| **Hand-pinning a digest** | a handful of updates a year, or hand-written manifests with no templating at all |
| **No tags at all** | `publish.tags` omitted entirely: the artifact is published by digest only, which is all image automation needs |
