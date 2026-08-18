# 0026. A source's artifact can lag its own spec, and the tag moved first

## Status

Accepted. Sharpens [0009](0009-flux-conventions-without-dependency.md)'s
`Reconciling`/`DependencyNotReady` path and answers the inverse of the hazard
[0017](0017-updating-the-consumed-digest.md) records.

## Context

An `ImageComposition` published a tag whose content came from the previous release.

The observation, concretely: tag `sf1ddb722b12a49a2` — the spec-hash tag generated for source
`ref.tag` `v0.6.8` — resolved to a digest whose layer contained `"version": "0.6.5"`. Nothing in
the object said so. It was found by pulling the artifact, extracting the layer, and reading the
payload.

### The mechanism

Every step is ordinary, which is why it took an extraction to find.

1. A generator bumps the `GitRepository`'s `spec.ref.tag` from `v0.6.5` to `v0.6.8` **and** rotates
   the composition's spec-hash publish tag, in one apply.
2. The composition's generation changes, so this controller reconciles **immediately**. The
   `GitRepository`'s generation also changed, but source-controller has to clone a new tag first,
   which takes seconds.
3. In that window the `GitRepository` is `Ready=True` — Flux does not tear down a good artifact to
   fetch a new one — with `status.artifact` still describing `v0.6.5`. `internal/source/flux.go`
   read `status.artifact.{url,digest,revision}` and treated it as authoritative. Nothing compared
   `metadata.generation` to `status.observedGeneration`.
4. The new tag does not exist yet, so `publishedState.matches` is false and the full build path
   runs.
5. The layer cache is keyed by the tarball's **content** digest, and that tarball is `v0.6.5`'s,
   already on disk. The build completes with no network access at all.
6. The immutable-tag guard checks whether the tag already resolves to something else. It does not
   resolve to anything. There is nothing to refuse.
7. `sf1ddb722b12a49a2` is published, pointing at `v0.6.5`'s content, permanently.

Permanently, because the next reconcile — with the source caught up — assembles the right content,
produces a different digest, and the guard now correctly refuses to move the tag. The object goes
`Stalled`/`ImmutableTagConflict` and stays there. The wrong tag cannot be corrected in place; only
a new tag, or `immutable: false`, moves past it.

### Why this is worse than 0017's hazard

0017 records the inverse and only the inverse: "It only holds for inputs pinned in the spec.
`sourceRef` resolves a Flux revision at reconcile time and `configMap` reads live content, so both
can change while the spec, and therefore the tag, does not." That is **content moving under a fixed
tag**, and 0017 is right that `immutable: true` surfaces it as a terminal error rather than silent
divergence.

Here the **tag moves ahead of the content**. The same guard is powerless, because a tag's FIRST
publish has nothing to conflict with — immutability is a statement about changing an existing
meaning, and this assigns a wrong meaning at the outset. The failure is silent at the moment it
happens and irreversible afterwards.

The guard nevertheless behaved correctly at every step, and it is the only reason this was ever
noticed: without it the tag would have been moved on the next reconcile and the incident would have
self-healed into an unexplained digest change nobody looked at.

## Decision

**1. A source's status is not believed unless it describes the source's current spec.**
`internal/source/flux.go` compares `metadata.generation` with `status.observedGeneration` and reads
the `Ready` condition before returning an artifact. Either check failing yields a typed
`ErrNotReady`, which `resolve.go` maps onto the existing `pendingError` — `Reconciling` with
`DependencyNotReady` and a 30-second retry. Not `Stalled`: source-controller catching up bumps no
generation on the composition, so stalling would wait for an event that cannot arrive (0009).

`Ready=False` is included for the same reason as the generation check rather than as a general
health gate: a source that failed its fetch keeps serving the last artifact it managed to get, so
consuming it means building from a revision the spec no longer names.

**Absent fields are not treated as failures.** These objects are read unstructured precisely so
this controller carries no dependency on source-controller's types (0009). Refusing on a field some
implementation does not publish would wedge every composition referencing it — a worse failure than
the one being prevented, and one this code could not diagnose. The check tightens what is knowable;
it does not demand it.

**2. Flux sources are watched.** Previously only `ImageComposition` and `ConfigMap` were, so a
source finishing its clone was invisible until `spec.interval` — an hour by default. With fix 1 in
place that would mean an hour of correctly refusing to build. The watch is an unstructured one per
kind, resolved through the manager's `RESTMapper` at startup and **skipped for any kind the cluster
does not serve**, because a cluster with no Flux installed must keep working. The mapping is
cluster-wide, unlike the `ConfigMap` one, because `sourceRef` carries an explicit namespace and
pointing at a shared source in `flux-system` is the ordinary arrangement.

## Consequences

**The watch is resolved once, at startup.** Installing Flux into a running cluster leaves this
controller without source watches until it restarts. Those compositions still reconcile on
`spec.interval`, so the failure mode is slowness, not incorrectness — correctness is fix 1's job,
and fix 1 does not depend on the watch.

**A composition can now wait where it previously built.** That is the point, but it is a behaviour
change: a source that never publishes `observedGeneration` matching its generation, or that sits
`Ready=False`, now holds its consumers in `Reconciling` indefinitely rather than building from
whatever it last published. The status message names the source and the reason, which is what makes
the wait diagnosable rather than mysterious.

**The `DockerBuild` controller shares `source.FluxSource` and was deliberately not changed.** It
sees `ErrNotReady` as an ordinary transient error: exponential backoff and a Warning event, rather
than the quiet fixed retry. Behaviourally safe — it also stops building from a stale context — but
noisier than it should be. Adapting its triage belongs with that controller's own work.

**Fix 1 does not cover a moving reference.** A `GitRepository` tracking a branch, or an
`OCIRepository` on a semver range, produces a new revision with **no generation bump at all**:
nothing about its spec changed. There is no window to detect, because from the API's point of view
the source was never behind. What the composition consumed is then whatever the artifact happened
to be at that instant, and if the publish tag was rotated for an unrelated reason in the same
apply, the same incident is possible. Fix 1 is precisely a fix for the *pinned-reference* case,
which is the one that occurred and the one a release generator produces.

### The follow-up not taken: an optional `sourceRef.revision`

The deeper fix is to let the composition state which revision it expects:

```yaml
sourceRef:
  kind: GitRepository
  name: app
  revision: v0.6.8          # optional; refuse anything else
```

The controller would compare it with `status.artifact.revision` — a field currently parsed in
`flux.go` and then consumed by nothing — and wait until they agree. That is the only variant that
also covers a moving branch or a semver range, because it stops inferring intent from the source's
metadata and takes it from the spec, where a generator can write it alongside the publish tag it
derives from the same value. It also makes the guarantee checkable rather than probabilistic: the
composition's tag and its content would be pinned to the same string by construction.

Not done here, because it is an API addition and this change had to be safe to ship immediately.
An API field needs its CEL rules, its interaction with the input hash (a declared revision that
moves the hash is what makes a rebuild happen at all), a decision about what a *mismatch* means —
almost certainly pending rather than terminal, since the source catching up is the fix — and
documentation of how a generator is expected to populate it. Recommended as the next step, with
0017's argument unchanged: an input that is not pinned in the spec cannot make the output tag mean
anything durable.

### The detectability gap

Nothing in this system recorded which source revision produced a build. `status.artifact.Revision`
is the **output** digest despite its name; `BuildRecord` stores tags, manifest and blob digests but
not even the `InputHash`; the published manifest carries no provenance annotations. So the question
"what went into `sf1ddb722b12a49a2`?" had no answer anywhere in the cluster, and was settled by
extracting a layer and reading a JSON file inside it.

Recommended, and not done here: record the resolved source revisions on the build — in
`status.history` next to the digest, where they survive as long as retention does. Two mechanisms
exist and they are not equivalent:

- **Config labels** (`spec.config.labels`) are part of the image config, so they change the output
  digest. Anything recorded there must go through `InputHash` or the short-circuit breaks, and
  adding them causes a one-time rebuild and republish of every artifact in the cluster.
- **Manifest annotations** do not change the config or the layers, but they *are* part of the
  manifest, so they change the manifest digest too — with the difference that they can be derived
  from data the hash already covers rather than adding new inputs to it.

Status is the cheaper half and should come first: it costs no digest churn at all, is visible in
`kubectl get -o yaml`, and would have answered the question in seconds.

## Alternatives rejected

**Stall on a stale source.** Wrong for the reason 0009 gives: the fix happens in another object and
raises no event here, so the composition would wait for a wake-up that never comes. This is the
exact mistake that was already made once with a missing source.

**Compare the artifact's revision against the source's `spec.ref` directly.** Would cover the
pinned case without an API field, and requires parsing source-controller's spec — `ref.tag`,
`ref.branch`, `ref.semver`, `ref.commit`, `ref.name`, and their `OCIRepository` and `Bucket`
equivalents — which is exactly the dependency on another controller's schema that 0009 refuses.
`observedGeneration` conveys the same information for the pinned case and is universal.

**Refuse to publish a tag that does not yet exist unless the source is verifiably current.** A
narrower version of fix 1 aimed only at first publishes. Rejected as strictly worse: the same
staleness with `immutable: false` silently moves a tag to old content instead, which is the
failure 0010 and 0017 are built to prevent, and the check would be conditional on a publish mode
rather than on whether the input is trustworthy.

**Disable the layer cache for `sourceRef` layers.** The cache made the incident invisible — no
network access, so nothing anywhere logged that old content was being used — but it did not cause
it. A cache miss would have fetched the still-published `v0.6.5` tarball from source-controller and
produced the identical wrong image, more slowly.
