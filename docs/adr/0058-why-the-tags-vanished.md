# 58. Why did those tags vanish?

## Status

**Open.** A leading hypothesis with a mechanism that fits, and no evidence from the cluster it
happened on. Nothing here should be cited as the cause.

## Context

The incident that prompted the retention work: **spec-hash tags disappearing minutes after a
successful push.** An `ImageComposition` published, reported Ready, and the tag it had just written
was gone shortly afterwards.

Several fixes have shipped since, and it matters to say clearly that **none of them was confirmed to
be this.** They were things found while looking, each defensible on its own:

| | what it was | would it explain this? |
|---|---|---|
| [ADR 0053](0053-a-publish-is-protected-before-the-reconcile-returns.md) | nothing refreshed a new artifact until the next scheduled cycle, up to an hour later | **as a contributing condition** — it is the window the others act in |
| [ADR 0054](0054-name-it-after-you-push-it.md) | a build's manifest was untagged while being named, and got collected | no — that is a *build*, and produces `NAME_UNKNOWN` on a fresh repository, not a vanished tag |
| [ADR 0057](0057-the-toolchain-is-an-input.md) | `keepTags`' `pushedWithin` rule was never evaluated | **this is the candidate** |

### The candidate

The chart configured two `keepTags` entries, one keying on `pulledWithin` and one on `pushedWithin`,
believing a tag survived if either kept it. zot stops at the first entry whose patterns match, both
matched `.*`, and so **only pull recency was ever in force.**

A tag that had been pushed and never pulled was therefore protected by nothing. That is exactly a
freshly published spec-hash tag, in the window between publishing and the refresher first reaching
it — the window ADR 0053 then closed.

It fits the symptom in the ways that matter: it affects **new** tags specifically, it is
**intermittent** (it depends on the collector reaching that repository first, and zot walks
repositories in rounds), and it leaves everything else alone, which is why nothing older was
affected.

### What does not fit, and is the reason this stays open

**The timing.** With the shipped `gcDelay` of `1h`, nothing is collectable until it is an hour old,
and the report says *minutes*. That is a real contradiction, not a detail. Either the cluster ran a
shorter `gcDelay` than the chart's default, or something else was at work, or the reported interval
is approximate.

The design doc's own analysis (§1.9) proposes a different mechanism — zot writing `PushTimestamp`
only when a digest has no Statistics entry, so a new tag on a known digest is born carrying an old
timestamp. That would defeat `pushedWithin` even after this is fixed, and it is **not** ruled out.
The two are compatible: this record does not displace it.

## What would decide it

From the cluster it happened on, in rough order of how much each would settle:

1. **The effective `gcDelay`.** From the running registry's config, not from what the chart would
   render today. If it was `1h`, the candidate is close to dead on timing and §1.9's mechanism
   becomes the leading one.
2. **Whether the vanished tags had ever been pulled.** The candidate requires that they had not.
   A single counterexample — a tag that was being pulled and still went — kills it.
3. **zot's own log lines** around the deletion, at `logLevel: info`. §1.9 of the design doc lists
   which lines discriminate its mechanism from this one. `test/e2e/retention_test.go`'s
   `registryLogs` shows the shape.
4. **Whether the digest was new or already known to the registry.** §1.9's mechanism needs a digest
   the registry had seen before; the candidate here does not care.

Until at least (1) and (2) exist, this stays Open. The repository has already been wrong twice about
this class of question by reasoning from a plausible mechanism instead of measuring — see
`test/e2e/up.sh` on the sweep interval, and ADR 0031's account of six wrong answers that each looked
like a finding.

## Consequences of leaving it open

**The fixes stand on their own.** Each was independently justified, so nothing has to be unwound if
the answer turns out to be something else. What must not happen is any of them being described as
*the* fix for this incident.

**The exposure is closed either way.** Under the candidate, the hole is fixed. Under §1.9's
mechanism, refresh-at-publish narrows the window to a single reconcile. Someone reading this later
should not conclude the cluster is still at risk — only that we do not know which repair was the one
that mattered.
