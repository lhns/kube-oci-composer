# 0019. Should garbage collection protect Pod-referenced digests?

## Status

**Open, and reopened twice.** It was built, measured to have no effect, and removed
([ADR 0011](0011-content-tags-expire.md)). It was reopened when [ADR 0013](0013-persist-manifests.md)
made the mapping it needed exist. That reasoning is now obsolete in turn:
[ADR 0035](0035-a-registry-is-the-only-publication-path.md) deleted the store 0013 built.

The **question** survives all three rounds. Only the shape of the answer keeps changing, which is
why this record is rewritten rather than closed.

## Context

**The gap, stated for the current design.** Retention keeps images alive by pulling them
([ADR 0031](0031-the-retention-guarantee.md)), and what it pulls is what a live `ImageComposition`
or `ImageBuild` references: its current artifact, plus `status.history` capped by `--keep-builds`.

A workload pinned to a digest **older than that cap**, or to one whose object has since been
deleted, is outside the set. Nothing pulls it, the registry's window elapses, and the image is
reclaimed. Threat D8.

**The failure is delayed and displaced, which is what makes it expensive.** Nothing breaks when the
image is deleted, because a running Pod does not re-pull. It breaks on the next reschedule, node
drain, or scale-up — as `ErrImagePull` for an image that worked yesterday, caused by a change
nobody made today. Raising `--keep-builds` widens the window; it does not close it.

### What the two earlier rounds got wrong, and why it matters now

**Round one built blob marking.** A Pod names a *manifest* digest, the embedded store held *blobs*,
and the only manifest-to-blob mapping was `status.history` — exactly what marking already read. It
protected nothing and was deleted.

**Round two reopened it because 0013's manifest store supplied that mapping.** Correct at the time,
and now moot: there is no embedded store to mark.

**The registry-only design makes the mechanism trivial.** Keeping a digest alive is now a `GET`.
There is no mapping to maintain, no marking, no sweep — the registry owns all of that. Two rounds
of this problem were about a data structure that no longer exists, and the remaining question is
purely: *which digests should be pulled, and what does it cost to find out?*

That is a much smaller question than the one this record has been carrying.

## Options

**A. A component that refreshes what Pods reference.** List Pods, collect image references whose
host belongs to a registry this operator manages, `GET` each digest.

- Closes the reschedule failure, which retention alone cannot.
- Costs cluster-wide `pods` read. That is the real objection — it is a broad grant, and neither
  controller has anything like it today.
- **Must read more than `containers[].image`.** `initContainers`, `ephemeralContainers`, and —
  centrally for this project — `spec.volumes[].image.reference`, which is how a composed artifact is
  consumed in the first place. Round one's implementation would have missed image volumes entirely,
  since they did not exist yet.
- **Must be scoped by registry host**, on the same rule as
  [ADR 0034](0034-a-default-registry.md): pulling every image every Pod references would mean
  authenticating to registries this operator has no relationship with.
- Still leaves the **scaled-to-zero hole**: a Deployment at zero replicas has no Pods, so its images
  are unprotected. Closing that means reading workload *templates* too, which widens the grant again.

**B. Leave retention as the only mechanism, and document the cap.** Cheapest. Turns a correctness
property into a capacity guess, and the guess is made by whoever sets `--keep-builds` without
knowing what any workload is pinned to.

**C. Protect by age as well as count.** "Keep anything published in the last N days" covers a
rescheduled Pod on a slow-moving artifact with no Pod access at all. Rejected once in
[ADR 0011](0011-content-tags-expire.md) as wrong in both directions — a busy object loses recent
builds, a dormant one keeps ancient ones — but that argument was about retention *replacing* the
count, not about an additional floor underneath it.

**D. Do nothing here, because the workload's owner should pin what it needs.** The honest version of
B: a digest a Pod depends on is that workload's dependency, and expecting the producer to guess how
long to keep it is the wrong direction. Weak where the same team owns both, which is the common case.

## The separate-component question

If A is chosen, it should be its **own Deployment, ServiceAccount and Role**, toggleable in the
chart alongside the other two ([ADR 0033](0033-one-chart-one-namespace.md)) — not folded into the
composer. Three reasons, in order of weight:

1. **RBAC.** Cluster-wide `pods` list/watch is the broadest grant in this system. The composer
   currently cannot create a single object and reads narrowly; giving it this would be the largest
   single widening of its authority since it was written. A toggle that removes the Deployment
   *and* its Role removes the grant.
2. **It is not this project's concern.** "Keep alive every image a Pod references" is true of any
   image in any registry with an expiry policy. Nothing about it is specific to `ImageComposition`.
   A capability that general does not belong inside a controller for one kind.
3. **Failure isolation.** Whatever this costs on a large cluster, it should not be able to stop
   compositions reconciling.

**It is probably not an operator.** It owns no CRD, writes no status and reconciles nothing — it is
a periodic sweep. A paginated `LIST` served from the API server's cache, once per interval, holds
nothing between sweeps, which removes most of the memory objection round one raised against an
informer. It may not even need to be a long-running process.

## What would decide it

- **Whether anyone has been bitten.** Still nothing observed. One occurrence settles it.
- **Whether A is a guarantee or a heuristic.** With the scaled-to-zero hole open it is best-effort,
  and must be advertised that way — a safety rail people believe in and that silently does not hold
  is worse than a documented cap.
- **The cost of the sweep on a real cluster**, which remains unmeasured, and is now a much smaller
  number than round one assumed.

## Consequences of leaving it open

The current behaviour is safe but blunt, and D8 names it plainly: retention follows live *objects*,
not live *workloads*. Anyone pinning a digest for longer than `--keep-builds` builds is relying on
something this system does not promise.
