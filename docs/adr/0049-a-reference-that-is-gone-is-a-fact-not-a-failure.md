# 49. A reference that is gone is a fact, not a failure

## Status

Accepted.

## Context

The same cluster as [ADR 0048](0048-retention-addresses-the-registry-it-can-reach.md) reported this,
hourly, for three days:

```
level=error logger=retention msg="refresh failed"
  object=freshrss/freshrss-ext-af-readability consecutiveFailures=71
  error="5 of 7 references: ...:sf0c42653b6f4d2de is gone: MANIFEST_UNKNOWN"

msg="retention refresh complete" objects=6 references=42
  refreshed=18 failed=6 notFound=18
```

Eighteen of forty-two references were already deleted. `refreshObject` counted each one as a
failure:

```go
case isNotFound(err):
    out.NotFound++
    failed++            // <- counts toward noteFailure
```

`failed > 0` calls `noteFailure`, so `consecutiveFailures` incremented every cycle and
`clearFailure` was unreachable. A deleted manifest cannot come back on its own, so the object stayed
Degraded permanently and the counter grew without bound.

The code already knew the difference. Three lines above:

> Counted separately because it is a different alarm from a registry that is merely unreachable: one
> says the protection failed, the other says it might.

It was counted separately in `Result` and then merged into `failed` immediately afterwards.

**The cost is not the counter.** `RetentionDegraded` is the warning that fires *before* deletion —
the refresh fails unsafe ([ADR 0031](0031-the-retention-guarantee.md)), so the alarm has to be
believable. An object permanently Degraded over history that expired weeks ago trains an operator to
ignore it, and a genuine registry outage then arrives looking like three days of existing noise.
That is exactly what happened here: ADR 0048's data loss was reported in the same words as the
backlog it had already produced.

## Decision

**Transient and permanent are counted apart, and alarmed apart.**

- `failed` counts only errors that might clear. `failed > 0` escalates through `noteFailure` as
  before.
- A gone reference increments `gone`, which does **not** feed the failure count. An object whose
  only problem is expired history reaches `clearFailure` and stops being Degraded.
- Gone references raise their own Warning, `RetentionLost`, saying how many of how many are gone and
  naming one.

**Reported every cycle, as a summary.** Once-per-reference would need per-object state that a
restart loses, and a loss that is still true an hour later is still true. A summary is one Event
regardless of how many references it covers.

**Gone references keep being refreshed.** A digest can be restored — an `ImageComposition`
re-publishes identical bytes on its next reconcile — and dropping them from the set would hide the
recovery as effectively as the old behaviour hid the loss.

### The stub could only produce one of the two

`recordingRegistry` could answer 404 and nothing else, so both tests about *transient* failure —
`TestSustainedFailureRaisesAnEvent` and `TestRecoveryClearsTheFailureCount` — were written using a
permanently deleted manifest. They asserted the escalation behaviour of a condition that can never
clear, which is the same conflation the production code made, encoded in the fixtures.

It can now answer 500, and both tests use that. This is the second time in two ADRs that a test stub
made a defect unreachable; the pattern is a fixture that cannot express the distinction the code is
supposed to draw.

## Consequences

**An object can now be healthy and lossy at the same time.** `Ready=True`, not Degraded, and an
hourly Warning saying some of what it published is gone. That is the honest description: nothing is
currently failing, and something was already lost.

**`RetentionLost` is a new reason string** and anything alerting on `RetentionDegraded` alone will
stop seeing these. That is the point, but it is a behaviour change for existing alert rules and the
changelog says so.

**A loss stays loud.** The Warning repeats every cycle for as long as the reference is missing. For expired history that is until the
record rotates out of `status.history`.
