# 52. A tag that cannot move needs a source that cannot either

## Status

Accepted. Extends [ADR 0026](0026-a-source-artifact-can-lag-its-own-spec.md), which closed the
half of this that is visible inside one object.

## Context

A field report: an `ImageComposition` permanently `Stalled=True reason=ImmutableTagConflict` after
Renovate bumped a `GitRepository`'s pinned commit. The tag published holds the *previous*
revision's content, the workload mounting it runs green, and no retry can correct the tag.

```
tag s0eff05b20f86b0e9 already resolves to sha256:1f4706...
but this spec produces sha256:61386e...
```

The report named two causes. **Both were wrong, and checking them is most of this ADR's value.**

### The layers are reproducible; provenance is what differs

The report concluded builds are non-deterministic and hypothesised mtimes leaking from
source-controller's tarball. `assemble.go` writes every entry with `ModTime: epoch`, empty
`Uname`/`Gname`, spec-supplied uid/gid, normalised mode, a stable sort with dedupe, under a
`gzip.Writer` whose own ModTime is zero; `cf.Created = epoch`, `cf.Author = ""`, `cf.History = nil`.
An mtime cannot reach a layer.

The digests differ because `provenanceAnnotations` records the source revision **on the manifest**.
Identical layer bytes plus a different revision is a different manifest digest, by design
(ADR 0033's provenance, threat R1).

This inverts the report's conclusion. It argued that reproducible builds would have made the
incident impossible — both pushes producing identical bytes, the second a no-op. The opposite
holds: **provenance makes the two certain to differ**, so the conflict is guaranteed rather than
unlucky. A fix resting on that premise would not have worked.

### The staleness guard was not misordered

The report says the guard "runs after the push instead of gating it". It does not. `checkCurrent`
is called inside `source.FluxSource` before anything is read out of status, and returns `Pending`
before any build. It is not reading a stale cache either: `cacheUnstructured` is left false, so
every unstructured read goes straight to the API server.

At the moment of the push the guard had nothing to see. The `GitRepository` was **self-consistent
at the previous generation** — spec, `observedGeneration` and `artifact` all describing `7630bee`.
The guard's message two seconds later belongs to a *later reconcile*, after the spec bump landed.

### The gap is between two objects, and no inspection of one can find it

A spec-hash tag ([ADR 0017](0017-updating-the-consumed-digest.md)) is computed by the **consumer**
from the spec, and folds in the referenced source's spec. So a pin bump changes the tag, and
kustomize applies the `ImageComposition` and the `GitRepository` from one commit, in kind order.
The composition's watch fires when its own object lands — which can be before the source's does.

In that window the new tag exists in the cluster and the source is entirely at the old revision,
consistent and `Ready=True`. Nothing about the source is wrong. What is wrong is that the
composition's tag has already moved and its source has not, and only the composition can know what
it was named for.

**The wedge is then permanent**: the tag is deterministic, so every retry reproduces the identical
conflict, and `Fail` correctly refuses each time.

## Decision

**A Warning when a layer names an unpinned `sourceRef` while the object publishes tags under
`Fail`.** Three conditions, all required:

- the layer names no `revision`, so it consumes whatever the source happens to hold;
- the conflict policy refuses to change what a tag means, so a second answer wedges;
- the push names at least one tag. **A digest-only publish is excluded**, because the name *is* the
  content: a different build gets a different name and collides with nothing. Warning there would
  be noise on the one configuration that is structurally safe.

**A warning and not a refusal.** Tracking a branch is legitimate, and ADR 0026 left the pin
optional deliberately. What was missing is that nothing said so until after it broke — and by then
the object shows a digest conflict with no trace of the unpinned source that caused it.

**The defence already existed and was not used**: `sourceRef.revision`, refused as `Pending` by
`RevisionMatches` until the artifact matches, and `--require-pinned-sources` to demand it
cluster-wide. This makes the gap between those and the default configuration visible.

## Consequences

**A legitimately branch-tracking composition under `Fail` now gets a recurring Warning.** The API
server aggregates by (reason, message, object), so it is one event with a rising count rather than
one per reconcile — but it is a new event on specs that were not previously flagged, and the way to
silence it is to pin or to choose a policy that tolerates movement.

**It does not close the race.** A user who ignores the warning can still wedge a tag. Closing it
requires the composition to state what it expects, which is what `revision:` is; the alternative —
deriving the tag from the observed revision — would break the spec-hash pattern, whose whole point
is that a consumer computes the tag without reading status.

**The conflict message still names two digests and no revisions.** Provenance records what produced
each, so a wedge could read "this tag holds revision X, this build is Y" instead. Worth doing;
deliberately not bundled here, because it needs a manifest GET on the conflict path rather than the
HEAD `ResolvePublished` does.

**Recorded so it is not rediscovered:** layer builds *are* reproducible, and the mtime hypothesis
was checked and is false.
