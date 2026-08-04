# 0011. Content tags expire: mark-and-sweep garbage collection

## Status

Accepted. Amends [0010](0010-workloads-reference-digests.md), which described content tags as
permanent.

**The auto-generated content tag itself no longer exists** — [0017](0017-updating-the-consumed-digest.md)
removed it in favour of consumer-chosen spec-hash tags. Everything here about *why* collection is
risky and how it is gated stands unchanged; only the name of what expires has moved. `status.history`
now records a list of tags per build rather than one content tag, and a build may have none at all.

The concern in the Context below is if anything sharper now: a spec-hash tag is what a rolled-back
commit names, so retention is what decides how far back a rollback can go.

## Context

Every build adds an immutable content tag and its layer blobs; every fetched layer source stays in
the cache. Nothing removed either, so both grew without bound.

Collection is not free of risk — it changes the failure mode from "runs out of disk eventually" to
"deletes something still in use", which is far worse. Most of this record is about that.

It also breaks a promise. 0010 offers hand-pinning a content tag as the supported fallback for
anyone not running image-automation, and that requires the tag to last. It cannot both last
forever and be collected.

## Decision

**Mark and sweep, gated on the controller's view being complete.**

`status.history` records each build: its content tag, its manifest digest, and the config and
layer digests it is composed of, newest first, capped at the retention limit. Recorded explicitly
rather than inferred from what is in storage — inference means concluding an object is garbage
because nothing *appears* to point at it, which is exactly the reasoning that deletes live data
when the view is incomplete. It also makes retention visible in `kubectl get -o yaml`.

Marking walks every `ImageComposition` and collects: every `spec.layers[].digest` (cache), every
retained build's blobs, and the currently published artifact. Sweeping deletes what marking did
not reach.

**Three rails, in order of importance:**

1. **Refuse to sweep when any object has not been reconciled.** Such an object contributes nothing
   to the live set, so its content is indistinguishable from garbage. Skipping costs one interval
   of growth; not skipping costs data. This reuses the readiness tracker, which already knows
   which objects have been through a reconcile. Failing to *evaluate* the gate is also a refusal,
   never permission.
2. **A grace period.** A build writes its blobs before recording them in status, so a sweep
   landing in that window would delete content moments from being referenced. Age is a crude proxy
   for "not mid-flight" but a safe one.
3. **A failed listing skips the namespace.** A partial listing reads as "these objects do not
   exist", which would make everything missing from it look unreferenced.

Plus `--gc-dry-run`, and **every deletion is logged**: a collector that silently reclaims is
impossible to audit after something goes missing.

**Retention defaults to 10**, overridable per object via `spec.publish.history`. Not 1, for three
independent reasons: reverting a commit must find the old digest still pullable; a pod pinned to a
digest that is rescheduled must be able to pull it again; and hand-pinning is the documented
fallback. Layers are shared between builds, so ten builds cost far less than ten times one.

## Consequences

**0010's permanence claim is now false**, and this record is why. A hand-pinned content tag lasts
for `history` builds of that object, not forever.

## Alternatives rejected

**Never collect.** Honest about 0010 and unbounded in storage. Rejected because "prune it by hand"
is not a design.

**Infer the live set from the store.** No way to distinguish "unreferenced" from "referenced by
something I have not looked at yet".

**Protect what a running Pod references.** Designed, **implemented, measured, and removed** —
because it protected nothing. A Pod names a *manifest* digest; the store holds *blobs*; and the
only manifest-to-blob mapping is `status.history`, which is exactly what marking already reads. A
digest whose record had aged out was deleted regardless. It would have cost a Pod informer and
cluster-wide `pods` list/watch for no effect.

That measurement pointed at the real gap: manifests are not durable at all (0013). Fixing that
would give the collector a manifest-to-blob mapping outliving `status.history`, at which point Pod
protection becomes implementable and worth revisiting.

**Time-based expiry instead of count.** Simpler, and wrong in both directions: a busy object loses
recent builds while a dormant one keeps ancient ones. Count tracks what "recent" actually means
per object.
