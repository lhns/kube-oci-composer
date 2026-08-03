# 0017. How does a workload's digest reference get updated?

## Status

**Open.** Nothing is decided. This is the most consequential unanswered question in the project,
because it is the one a user hits on day one.

## Context

[ADR 0010](0010-workloads-reference-digests.md) settles what a workload *references*: a digest,
never a tag. It does not settle how that digest gets into git, and the assumption it made —
"Flux image-reflector and image-automation close the loop" — turned out not to hold for the
cluster this was built for.

Two things came to light:

1. **Flux image automation is frequently not installed.** A stock `flux install` ships source,
   kustomize, helm and notification. The image-reflector and image-automation controllers are
   opt-in, and the estate this was written for does not run them. Building on their presence is
   building on an assumption.
2. **The inputs are as much of a problem as the output.** Renovate and similar tools discover
   images by recognising known structures — pod specs, Flux kinds. Nothing recognises
   `spec.base.image` inside a CRD it has never heard of. So a naive setup leaves the user
   hand-tracking upstream base image releases *as well as* hand-updating the output pin: two
   manual steps per upstream release, which is precisely the coupling the project exists to
   reduce.

There is also an inconsistency worth resolving either way: we tell consumers to reference
`repo:tag@sha256:…`, while the API splits that into `base.image` plus `base.digest`.

## Options

**A. Renovate, with the inputs made discoverable.** Merge `base.image` and `base.digest` into one
conventional `ref: repo:tag@sha256:…`, which Renovate's docker datasource updates natively, and
document a regex `customManager` for `fetch.url` + `fetch.digest`. The consumer's reference is
then updated by the same mechanism, since it is an ordinary image reference.

- Needs no new controllers, and reuses a tool most GitOps repos already run.
- Renovate typically runs in CI, so it must be able to *reach* the serving endpoint. For the
  built-in endpoint that means exposing it publicly, which some users will not want.

**B. Flux image-reflector plus image-automation.** The originally assumed path.

- Runs in-cluster, so the endpoint never needs public exposure.
- Two more controllers, three more CRDs, and git write credentials for Flux.
- Duplicates Renovate where Renovate is already in use.
- **Unverified.** `ImagePolicy` needs a selection rule, and our tags are a fixed moving pointer
  plus content tags with an unordered digest suffix. Whether `filterTags` down to a single
  candidate plus `digestReflectionPolicy: Always` behaves sensibly has not been tested. Neither
  has the marker syntax for substituting a digest.

**C. Hand-pinning.** Read `status.artifact.contentTag`, commit it.

- Works today with nothing installed.
- Fine at a few updates a year; poor at a few a week.
- Does not solve the input problem at all — the base digest still has to come from somewhere.

These are not exclusive. A is the likely default with C as the floor; B matters for anyone who
cannot expose the endpoint.

## What would decide it

- **Does `ImagePolicy` work against our tag layout?** A test against a real image-reflector,
  scanning a real serving endpoint. This has been flagged three times as the integration point
  most likely to be subtly wrong and remains untested.
- **Is the endpoint publicly reachable in practice?** If exposing it is normal, A dominates. If
  it usually is not, B has to be supported properly rather than assumed.
- **Whether the single `ref` field survives contact with the other verbs.** `fetch` has no
  standard combined form, so it keeps `url` + `digest` regardless — meaning the API would be
  consistent with convention but not internally uniform. That may be the right trade; it has not
  been argued through.

## Consequences of leaving it open

The README and ADR 0010 currently present Flux image-automation as the expected path. That is
misleading for anyone without those controllers, and should be corrected before the first release
whichever way this lands.
