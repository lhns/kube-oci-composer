# 31. Live objects' images are never reclaimed, by anything

Date: 2026-08-19

## Status

Accepted. Depends on [0030](0030-a-real-registry-serves-both-kinds.md), which makes a real registry
the serving surface. Sharpens [0011](0011-content-tags-expire.md) (content tags expire) and depends
on [0010](0010-workloads-reference-digests.md) (workloads reference digests), which is why untagged
is not the same as unreferenced.

This is the load-bearing record of the registry work. Everything else in it is arrangement; this is
the property that must hold.

## Context

Handing retention to a registry hands it a delete button pointed at content this operator's
workloads are running. `status.history` stops being the thing that keeps an image alive and becomes
merely a description of what once existed, unless something connects the two.

The requirement is not "expiry should be conservative". It is absolute:

> **An image named by the retained `status.history` of any live `ImageComposition` or `ImageBuild`
> is never deleted, by anything.**

Expiry beyond that is best-effort. **Leaking bytes is acceptable; losing live content is not.** The
failure mode being excluded is a rescheduled pod that cannot pull the digest it is pinned to,
because something reclaimed it while the object naming it was healthy and running.

That asymmetry is what decides every question below. Where a mechanism can fail in two directions,
the design takes the one that leaks.

### Why the obvious answer is the wrong one

The natural design is: when a `BuildRecord` falls out of `status.history`, delete that manifest.
Deterministic, and it fails safe — a controller that is down deletes nothing.

It has two defects that are not fixable by care:

- **Two objects publishing the same digest.** Composition is deterministic, so this is ordinary
  rather than exotic. One object's eviction destroys the other's content unless there is
  cross-object marking, which is a distributed mark-and-sweep with all of its failure modes.
- **The controller needs delete permission.** Every bug in it is then potentially destructive, and
  the guarantee rests on the correctness of the component most likely to have bugs.

Neither is decisive alone. Together they say the mechanism is the wrong shape: it makes *deletion*
the action, when what the system actually knows is which images are *alive*.

## Decision

**Liveness is asserted continuously and positively, never inferred from the absence of a
reference.**

Every live object periodically **touches** the images its retained `status.history` names, under
both their digests and their tags. The registry expires anything not touched within a window many
times longer than the touch interval: 30 days against an hourly refresh is a 720× margin.

"Still referenced" becomes a **lease**, renewed by the object that holds it, rather than a
conclusion drawn from a scan.

### The refresh is a PULL, not a push

This is the part worth stating loudly, because the design was planned around re-*pushing* manifests
and the measurement changed it.

zot expresses retention over both push and pull recency (`pushedWithin`, `pulledWithin`,
and `keepUntagged` for manifests with no tag). A **read** therefore renews the lease.

That is better on every axis that matters here:

- **The controller needs no write access to keep anything alive**, and no delete permission either.
  It cannot destroy an image by any call it makes. This is the strongest form of the constraint that
  nothing but the controllers may write to the registry — the refresh path does not even have that
  much authority.
- **Nothing is transferred.** A manifest is a few KB; blobs are untouched.
- **It cannot corrupt what it protects.** A write that goes wrong can leave the wrong bytes under a
  name. A read cannot.

**A refresh is emphatically not a rebuild.** For `ImageBuild` that distinction is load-bearing: a
rebuild is non-deterministic and could produce a *different* digest, so a refresh that rebuilt would
destroy the very thing it exists to preserve, and burn a build pod every cycle doing it.

### Four conditions, each verified rather than assumed

Each of these can silently defeat the guarantee, and silence is the failure mode that matters.

1. **A pull must actually renew recency, and it must be a `GET`.** If it does not, the refresh is a
   no-op and live images expire quietly. This is the assumption the whole design rests on, and it is
   measured against the bundled version in `test/e2e/retention_test.go` — with a negative control,
   because a registry that never deletes anything satisfies the guarantee vacuously and looks
   identical to one that works.

   **A `HEAD` does not count, and that is measured rather than assumed.** The first run of that file
   polled a tagged manifest with `HEAD` and polled an untagged one with `GET`; the `HEAD` case was
   collected while being polled every five seconds, and the `GET` case survived past two windows.
   Reasonable behaviour — an existence check is not a pull — but it is a trap: `HEAD` is the natural
   thing to reach for when the question is "does this still exist", it is cheaper, and a refresh
   built that way does *nothing at all* while looking correct in every log. The symptom would arrive
   one retention window later, as missing images.

   This is the single most valuable thing the measurement produced, and it is why the plan called
   for measuring rather than reading documentation.
2. **Refresh is driven by `status.history`, never by a successful reconcile.** An object Stalled on
   a spec error must keep refreshing what it already published; those images may be running right
   now. This is the most likely implementation mistake, and stalling is exactly when an operator is
   least able to notice.
3. **Digests are refreshed, not only tags.** [0010](0010-workloads-reference-digests.md) makes
   digest references the recommended usage, so an untagged manifest may be what a running pod pulls.
   Untagged is not unreferenced.
4. **Refresh failure is loud.** A design that fails *unsafe* needs monitoring in a way that a
   fail-safe one does not. Sustained refresh failure must raise a condition and an event well before
   the window elapses, because the alternative to noticing is deletion.

### What the collector becomes

`internal/gc` does not disappear. Its **marking** — walking every live object of both kinds to
decide what must be kept — becomes the refresh's input. What disappears is the sweep, the store, the
blob and manifest namespaces, and the need for any delete call at all.

`Readiness.Pending` stops being a rail against deletion and becomes one against a *partial refresh*:
an incomplete view of the live set must not silently under-refresh, because under-refreshing is
indistinguishable from eviction and arrives 30 days later.

## Consequences

**Retention policies on the registry may be enabled, and only because of this.** Without a refresh,
the honest answer would be to disable expiry entirely and accept unbounded growth. The lease is what
makes bounded storage compatible with the guarantee.

**Eviction needs no action, and gains an undo window.** A record falling out of `status.history`
simply stops being refreshed. The expiry window doubles as a recovery period: a mistaken eviction is
reversible for as long as it lasts, which delete-on-eviction never offers.

**Two objects publishing the same digest solves itself.** Both refresh it; it survives while either
lives. No cross-object marking, no ordering, no coordination.

**A controller that is down for longer than the window loses content.** This is the price of a
fail-unsafe design and it should not be understated. It is mitigated by the margin — a 720× ratio
means an outage must last weeks, not hours — and by condition 4, which is what turns a long outage
into an alert rather than a discovery. An operator who cannot tolerate that should disable registry
retention entirely; the guarantee then holds trivially and storage grows.

**The window is now a correctness parameter, not a preference.** It must stay far larger than the
refresh interval, and anyone tuning either has to move both. That relationship belongs in the
documentation next to the values, not in a comment.

**`ImageBuild` makes the registry a system of record.** Its content cannot be rebuilt
([0025](0025-dockerfile-builds-as-a-second-kind.md)), so for that kind this guarantee is the only
thing standing between an eviction bug and permanent loss. Registry storage must be backed up;
nothing else holds those bytes.
