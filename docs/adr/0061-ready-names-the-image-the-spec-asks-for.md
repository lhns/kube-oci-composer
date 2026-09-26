# 61. Ready names the image the spec asks for

## Status

Accepted. Sharpens [0009](0009-flux-conventions-without-dependency.md).

## Context

A Flux Kustomization applied an `ImageBuild` change with `wait: true`. The object reported
`Ready=True, Succeeded` at the new generation within seconds, while its build ran for another two
minutes and `status.artifact` still named the previous image. The Kustomization counted it done.
Anything ordered after it could have rolled pods onto the old digest; that time the timing was lucky.

The builder set `observedGeneration` and `Ready=True` on every pass that did not fail, including the
pass that started a Job and every pass that found it still running. To kstatus, which is what
`wait: true` uses, that reads as finished.

`ImageComposition` had the same gap, shorter. It assembles within one reconcile and wrote status only
at the end, so the previous pass's `Ready=True` stood while it fetched, assembled and pushed. After a
spec edit kstatus is saved by `observedGeneration` lagging. After an edited ConfigMap or a moved
source revision it is not: the generation never changes, and only the conditions can say "wait".

## Decision

**`Ready=True` means the image the current inputs produce is published.** Work in flight is
Flux's `Ready=Unknown` with `Reconciling=True`, both reason `Progressing`. The message names the
image still published, since that is what a consumer gets until the work finishes.
`recon.SetProgressing` writes it for both kinds.

- **ImageBuild:** a build is in flight while `status.buildRef` is set: from the Job's creation until
  it succeeds, fails or its tag is kept. A running Job found without one (a status write lost after
  `Create`, a restart) is recorded as it. `observedGeneration` still advances on every pass, because
  the retention refresher skips its whole cycle while any object lags, and a build can run for
  minutes. `Reconciling=True` keeps kstatus reporting in progress regardless.
- **ImageComposition:** once the converged check has failed, and before anything is fetched,
  one status write sets Progressing. It leaves `observedGeneration` and `lastHandledReconcileAt`
  alone. Both mean a pass is over, and a `flux reconcile` waiter must not see its request handled
  halfway through. The pass is seconds long, so the refresher's gate loses nothing.

A converged pass writes no Progressing, so an interval reconcile never flickers.

## Consequences

- `flux` `wait: true`, `kstatus`, and `kubectl wait --for=condition=Ready` once `observedGeneration`
  has caught up, all hold until the new image is published.
- A rebuild that turns out to reproduce the published digest still shows Progressing while it
  assembles. The controller did not know until it had assembled.
- Nothing inside the controllers read `Ready` before or after. Compositions consume
  `status.artifact`, which a build in flight never touches.

## Alternatives rejected

- **`Ready=False` while building.** It reads as failure, and Flux's own controllers use Unknown
  for in progress. Dashboards and alerts on `Ready=False` would fire on every rebuild.
- **Hold the builder's `observedGeneration` back until the build finishes.** kstatus would wait, but
  so would the retention refresher, cluster-wide, for as long as any build runs. That is the stall
  a suspended `ImageBuild` caused before 0.6.0. It also would not cover an input change, where the
  generation does not move.
- **Only document it** ("wait on `status.artifact.digest`"). The consumer rarely knows the digest to
  wait for; that is what it is waiting to learn.
