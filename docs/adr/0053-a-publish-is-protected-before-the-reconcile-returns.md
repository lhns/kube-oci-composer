# 53. A publish is protected before the reconcile returns

## Status

Accepted. Extends [ADR 0031](0031-the-retention-guarantee.md), whose lease is renewed on an
interval and, until now, only on one.

The zot mechanism cited below as the diagnosis of the live-cluster incident is **not** confirmed to
have been it: [0058](0058-why-the-tags-vanished.md) records that incident as still unexplained.
This record stands on its own -- the window it closes was real whatever caused the loss.

## Context

A live cluster reported spec-hash tags vanishing minutes after a successful push, reproducibly,
while the consumer still referenced them.

ADR 0031 makes liveness a **lease**, renewed by pulling. What it did not say is when the first
renewal happens. `RefreshOnce` runs on a ticker and nothing touches a newly published artifact, so
between the push and the next cycle — a full `refreshInterval`, an hour by default — **the artifact
has no lease at all.**

A registry expiring on pull recency holds no record that anything was pushed. Worse, per an
analysis of zot v2.1.20 from the reporting cluster, zot writes `PushTimestamp` only when a digest's
Statistics entry is absent while retention builds candidates per *tag* — so a new tag on a digest
it has seen before is **born carrying an old timestamp**. A collection pass inside that hour
reclaims content that is minutes old, and does so deterministically rather than as a race.

### A second hole, in the same place

`RefreshOnce` refuses to run when any object has `observedGeneration != generation`, and returns
`Skipped`. The gate is right — [ADR 0031](0031-the-retention-guarantee.md)'s condition 2 is that a
partial view must not under-refresh — but the *reporting* was not. `cycle()` logged a skip at
`Info`, while a **failed** cycle was escalated and carried a comment about being a step toward
deletion.

The consequence is identical: nothing was refreshed. And a skip is the more dangerous of the two,
because it does not resolve on its own — one object stuck behind its generation stops the refresh
for **every object in the cluster**, indefinitely, saying so only at Info.

## Decision

**A publish renews its own lease before the reconcile returns.** `Refresher.RefreshNow` refreshes
one target immediately; both controllers call it after a successful publish.

- **Not gated on `Pending`.** That gate protects against acting on a partial view; here the caller
  holds one object it has just published and knows to be current.
- **Never fatal.** The push succeeded. A failed opportunistic refresh leaves exactly the situation
  that existed before the call, and the next cycle retries.
- **Only on a real publish.** The composer keys on `result.Record != nil`, which is already how it
  distinguishes a publish from a converged no-op; refreshing on every reconcile would pull every
  artifact hourly for nothing.
- The artifact is **passed in**, not read back from the object: `patchStatus` mutates a freshly
  fetched copy, so the object the reconcile holds still describes the previous pass. A test caught
  this, having been written to fail first.

**A skipped cycle escalates like a failed one.** Consecutive skips are counted and, past
`DegradedAfter`, logged at `Error` naming what is pending. Logged rather than raised as an Event,
because the condition is cluster-wide and attaching it to one object would misattribute it.

## Consequences

**`RefreshNow` is called from a reconcile while the cycle goroutine may be running**, so the
consecutive-failure map is now mutex-guarded. It was previously touched only by the ticker.

**One extra manifest GET per publish.** Negligible against a build, and it is the same request the
hourly cycle would have made.

**The gap is closed, not the underlying defect.** zot still freezes `PushTimestamp` per digest.
What this guarantees is that a lease exists from the moment of publication; a registry that ignores
pulls entirely would still expire content, which is what ADR 0031's measured condition 1 exists to
catch.

**A skip that lasts is now audible.** An operator who has left an object unreconciled will see an
error every cycle rather than an Info line. That is the intent: it is not a benign state.
