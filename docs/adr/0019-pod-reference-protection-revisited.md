# 0019. Should garbage collection protect Pod-referenced digests?

## Status

**Open, and reopened.** It was built, measured to have no effect, and removed
([ADR 0011](0011-content-tags-expire.md)). The reason it could not work has since been fixed for
unrelated reasons, so the question is live again.

## Context

Retention is currently the only thing keeping an old build pullable: `--gc-keep-builds` (default
10) decides how many past builds survive per object. A workload pinned to a digest older than that
cannot pull it after being rescheduled, and the failure appears somewhere unrelated to the change
that caused it.

Protecting anything a Pod currently references is the obvious fix, and it was implemented. It
protected nothing, for a specific reason: a Pod names a **manifest** digest, the store holds
**blobs**, and the only manifest-to-blob mapping was `status.history` — which is exactly what
marking already reads. A digest whose record had aged out was reclaimed regardless. Rather than
ship a safety rail that did nothing, it was deleted.

**What changed:** [ADR 0013](0013-persist-manifests.md) added a `manifests/` namespace so builds
survive a restart. That store now holds the manifest bytes for every retained build, and a
manifest lists its own config and layer digests. The mapping the feature needed exists — it just
was not built for this.

## Options

**A. Reinstate it, reading the mapping from the manifest store.** For each Pod-referenced digest
under the serving host, load the stored manifest and mark its blobs.

- Fixes the failure that retention alone cannot: a pinned digest surviving a reschedule.
- Costs a Pod informer and cluster-wide `pods` list/watch — real RBAC and memory, and the reason
  it was flagged as expensive the first time.
- Only protects what is *currently* referenced, so a Pod that is scaled to zero and later scaled
  up is still exposed. That may make it a partial rail rather than a guarantee, which is worth
  being honest about before advertising it.

**B. Leave retention as the only mechanism, and raise the default.** Simpler and cheaper. Turns a
correctness property into a capacity guess.

**C. Protect by age instead of count.** "Keep anything published in the last N days" covers a
rescheduled Pod on a slow-moving artifact without watching Pods at all. Rejected once already in
ADR 0011 as wrong in both directions — a busy object loses recent builds, a dormant one keeps
ancient ones — but that argument was about *retention*, not about an additional floor.

## What would decide it

- **Whether anyone has actually been bitten.** The failure is real but has never been observed,
  because nothing is deployed. One occurrence would settle it immediately.
- **Measuring the Pod informer cost on a real cluster.** The objection is memory and RBAC breadth;
  neither has been quantified.
- **Whether A is a guarantee or a heuristic.** If the scaled-to-zero hole stands, it should be
  documented as best-effort, and B or C may be the more honest answer.

## Consequences of leaving it open

The current behaviour is safe but blunt: retention is a number a user has to reason about, and
getting it wrong shows up as an unpullable digest much later. The package documentation records
why the rail was removed, which will read as stale once someone notices the manifest store exists.
