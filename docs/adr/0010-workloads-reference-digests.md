# 0010. Workloads reference digests, never tags

## Status

Accepted. The permanence claim below is amended by [0011](0011-content-tags-expire.md) and
[0013](0013-persist-manifests.md) — read those before relying on it.

## Context

A Pod spec cannot read another object's status, so the reference to a composed artifact is a
literal string somewhere in git. How that string is chosen is the single most consequential
decision in the project, because both obvious answers are bad:

- **An immutable tag.** A routine edit — fixing a path, adding a file — leaves the object unable
  to publish until someone invents a version number. Annoying, and it can wedge.
- **A mutable tag.** Nodes with `IfNotPresent` keep stale bytes, and you cannot tell which pod is
  running which content. Silent, and the worst kind of bug.

Injecting the reference does not work either. A controller rewriting the workload fights Flux,
while a webhook mutating *Pods* would not fight Flux but also would not trigger a rollout — so
nothing would pick up a new build.

## Decision

**No workload ever references a tag. The tag is for automation; the pod references a digest.**

Mutability stops mattering once nothing consumes the mutable thing.

Every build publishes **two** references:

1. **An immutable content tag**, `<tag>-<digest[:12]>`, never reused for different content.
2. **A moving pointer**, `spec.publish.tag`, repointed at the newest build. It exists solely for
   automation to watch and is never named by a workload.

Flux closes the loop: an `ImagePolicy` with `digestReflectionPolicy: Always` resolves the pointer,
and `image-automation` writes the digest into git behind an `$imagepolicy` marker.

```yaml
volumes:
  - name: plugins
    image:
      reference: oci.example.com/plugins:main@sha256:abcd…  # {"$imagepolicy": "flux-system:plugins:digest"}
      pullPolicy: IfNotPresent
```

Against each objection:

| objection | answer |
|---|---|
| manual version bumps are annoying | none needed — edit layers, done |
| controller rejection can jam | nothing to reject; the pointer is meant to move |
| mutable tags are horrible | agreed, and no workload consumes one |
| will the pod pull the right bytes? | it references a digest, so `IfNotPresent` is exactly correct |
| will it notice a rebuild? | the digest in the pod template changes, so a rollout happens |

Git stays the source of truth for what is *deployed* — the digest is committed and reviewable —
while the controller owns what is *available*.

## Consequences

It needs `image-reflector` and `image-automation` running, and Flux needs git write access. There
is lag: build → scan interval → commit → reconcile, so it is eventually consistent rather than
instant.

**For anyone not running image-automation**, the immutable content tag is the documented fallback:
pin `<tag>-<digest[:12]>` by hand. Correct, just manual.

**That fallback is weaker than originally claimed**, in two ways found after the fact:

- Content tags are garbage collected once they fall outside the retention window (0011).
- Older manifests do not currently survive a controller restart at all — measured, and true of
  digest references as well as content tags (0013).

Both are recorded rather than quietly dropped, because this fallback was offered as a supported
path and anyone relying on it deserves to know its limits.

A `scripts/validate.sh` lint should assert that no workload references a bare moving pointer
without a digest, turning the remaining footgun into a failed PR check.

## Alternatives rejected

**Immutable tags only.** Wedges on a routine edit, as above.

**Mutable tags consumed directly by workloads.** Silent staleness with `IfNotPresent`, and no way
to tell what is running.

**A mutating webhook that rewrites Pod images.** Does not fight Flux, but a Pod-level mutation
does not change the Deployment's pod template, so nothing rolls out and new builds are never
picked up.

**A controller that patches the consuming workload.** Fights Flux directly: the Kustomization
reverts it on the next reconcile, forever.
