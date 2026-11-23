# 63. A partial view is refreshed

## Status

Accepted (2026-11-23). Amends [0031](0031-the-retention-guarantee.md) and
[0053](0053-a-publish-is-protected-before-the-reconcile-returns.md).

## Context

0031: "`Readiness.Pending` stops being a rail against deletion and becomes one against a *partial
refresh*: an incomplete view of the live set must not silently under-refresh". 0053: "The gate is
right ... but the *reporting* was not."

The gate came from the mark-and-sweep collector, where a partial view deleted things. In a lease it
only withholds a read: targets come from the API list with their status, and an unreconciled
object's status is still exactly what it published (0031, condition 2). Refreshing a partial view
protects strictly more than refreshing nothing, and a refresh is a GET, so it cannot protect the
wrong set. What is worth keeping is "not silently".

The gate also caused defects. In 0.6.0 one suspended ImageBuild stopped the refresh for every
image. `Readiness` is in memory and the refresher runs a cycle at start, so the composer's first
cycle after every start refreshed nothing; a composer restarting more often than the refresh
interval never refreshed and, with an in-memory skip counter, never escalated.

## Decision

**`RefreshOnce` never skips.** It refreshes every object from its status, returns the unreconciled
names in `Result.Pending`, and the cycle logs them. An object unreconciled for `DegradedAfter`
consecutive cycles is logged at error; different objects pending in different cycles never
escalate. A `Pending()` list error, or no lister configured, still fails the cycle. Anything an
unreconciled object publishes later is protected by `RefreshNow` in its reconcile (0053, unchanged).

## Consequences

An unreconciled object no longer costs anyone protection. "Refresh complete" no longer implies every
object was reconciled; the `unreconciled` field says how many were not. Escalation is a log, not an
Event, because `PendingLister` yields names only.

## Alternatives rejected

**Keep the gate and shorten the interval.** Still refreshes nothing after each start, and still lets
one stuck object block every other.

**Exempt only new objects.** Cannot tell a new object from a stuck one, and leaves the 0.6.0 defect.
