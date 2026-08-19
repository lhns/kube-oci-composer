# 29. A tag conflict has three answers, not two

Date: 2026-08-19

## Status

Accepted. Amends [0017](0017-updating-the-consumed-digest.md):79-97, where `immutable` defaults to
true and is what makes referencing a tag safe. Touches
[0010](0010-workloads-reference-digests.md) and [0011](0011-content-tags-expire.md).

Records, separately, that `push.immutable` was **inert on `ImageBuild`** from the day that kind
shipped ([0025](0025-dockerfile-builds-as-a-second-kind.md)) until this change.

## Context

### The field that did nothing

`ImageBuild`'s CRD carried `spec.push.immutable`, defaulted to `true`, documented as refusing to
move a tag that already resolves to different content. Nothing in `internal/buildcontroller/` read
it. BuildKit was handed `type=image,...,push=true` and overwrote whatever the tag held.

So an operator who set `immutable: true` — or who simply accepted the default, which is everyone —
believed a tag could not be silently remeaned, and it could. That is worse than not offering the
guarantee: an absent feature is visible, a lie is not. This record exists partly so that the gap is
written down rather than quietly closed.

The check has to happen **before the Job is created**, and that ordering is the whole substance of
the fix. BuildKit pushes from inside the Job, so by the time the controller observes a result the
tag has already moved. There is no undo. A conflict noticed afterwards is a conflict that has
already happened.

### Two values were the wrong shape

`immutable` is a boolean, so it can say *refuse* or *overwrite*. Neither is right for the pattern
this project actually recommends — a tag derived from a hash of the spec, as in
`docs/examples/spec-hash-tag/`. There, a tag that already exists means **the content is already
published and correct**:

- *Refuse* stalls the object over a non-problem. The tag holds exactly what it should.
- *Overwrite* rewrites bytes that were already right. On `ImageComposition` that is merely wasteful,
  since the output is a function of the spec. On `ImageBuild` it is worse: the output is **not** a
  function of the spec ([0025](0025-dockerfile-builds-as-a-second-kind.md)), so overwriting replaces
  good content with a *different* build of the same inputs.

The missing third answer is *leave it alone*.

## Decision

**`onConflict` on both `Publish` and `Push`**, with the same three values and the same meaning:

| Value | Behaviour |
|---|---|
| `Fail` | Refuse and stall. The default. |
| `Overwrite` | Move the tag. |
| `Keep` | Leave the existing tag, drop what this reconcile produced, report Ready. |

The field is identical on both kinds, deliberately. An operator moving between them should not have
to learn a second set of semantics for the same question.

**`Keep` must record the divergence.** `status.conflict` names the tag, what it resolves to, what
was dropped, and when. Without it the object reads Ready while *not* having published what its spec
produces, and nothing anywhere says the two disagree — which is precisely the shape of the incident
behind [0026](0026-a-source-artifact-can-lag-its-own-spec.md). The record is cleared as soon as a
reconcile publishes cleanly, so it describes the current state rather than accumulating history.

The two kinds fill it in differently, and the difference is not an oversight:

- On `ImageComposition` the artifact is built before the conflict can be detected, so `dropped`
  holds a real digest, and `status.artifact` reports the **existing** digest — what a consumer
  actually pulls.
- On `ImageBuild` no build is run at all, so `dropped` is empty and `status.artifact` is left
  untouched. Synthesising one from what the tag holds would assert that this object produced that
  content, which on this kind is exactly what cannot be known.

**`immutable` is deprecated, not removed.** It is honoured when `onConflict` is unset: `true` means
`Fail`, `false` means `Overwrite`. Precedence is `onConflict`, then `immutable`, then `Fail`.

**`onConflict` carries no schema default, and `immutable` loses the one it had.** This is the least
obvious decision here and the one most likely to be "fixed" by someone later.

Structural schema defaults are applied when an object is **read back from storage**, not only when
it is written. Defaulting `onConflict` to `Fail` would therefore rewrite every stored
`immutable: false` object into a refusing one the moment the CRD was upgraded — silently reversing a
setting its author chose on purpose. And keeping `immutable`'s default would materialise it under
every new object, making an explicit setting indistinguishable from a defaulted one, which leaves no
validation rule able to tell a contradiction from an accident.

So the effective default lives in Go, in `ResolveConflictPolicy`, where it can consult `immutable`
first. Removing the schema default changes nothing for stored objects, whose value is already
written, and nothing for new ones, which resolve to `Fail` either way.

**Contradictions are refused by CEL, agreements are not.** `immutable: true` with
`onConflict: Overwrite` is rejected at admission. `immutable: true` with `onConflict: Fail` is
accepted, because every object stored before this release already carries `immutable: true`
materialised by the old default, and a rule refusing mere co-presence would stop all of them from
adopting the new field without a second edit.

## Consequences

**A build can now be refused before it runs, which is a behaviour change on `ImageBuild`.** An
object whose tag already holds someone else's content, and which previously overwrote it without
comment, now stalls. That is the point — but it will surface on upgrade as objects going
`Stalled` that were "working" before, and the release notes have to say so.

**The builder now talks to a registry itself.** It resolves what its tags hold before creating a
Job, using the same `spec.push.secretRef` the Job is given. Its RBAC is unchanged: `get` on secrets
only, no `list` and no `watch`, so it cannot enumerate a namespace's credentials even in principle.

`Overwrite` deliberately skips that lookup. There is nothing to decide, and skipping it keeps the
permissive policy working when a registry is unreachable for reads but writable for pushes — which
would otherwise be a regression for every object that already used `immutable: false`.

**"Is this a conflict?" is a different question on the two kinds.** The composer compares the tag
against the digest it just produced. The builder cannot: it does not know what the build will
produce until it has run. So it asks whether the tag holds anything *other than this object's own
recorded digest*. A tag pointing at its own last build is not a conflict; anything else is.

That is a coarser test, and it is coarse in the safe direction. It can refuse a build that would
have produced identical bytes; it cannot permit one that silently remeans a tag.

**The shared machinery moved.** `Published`/`ResolvePublished` and the dockerconfigjson keychain now
live in `internal/reconciler`, imported by both controllers. That does not weaken
[0004](0004-two-kinds-two-controllers.md)'s separation, for the reason that record already gives:
it separates *components*, and a shared library is not a shared controller.

**`Keep` makes a new kind of drift possible, and this is its cost.** An object can sit Ready
indefinitely while its spec describes content that was never published. `status.conflict` and the
Ready message are the only things standing between that and an operator's incorrect belief, so both
must stay legible — a conflict an operator has to go looking for is one they will not find.
