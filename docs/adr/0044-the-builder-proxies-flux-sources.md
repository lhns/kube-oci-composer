# 44. The builder proxies Flux sources

## Status

Accepted.

## Context

A build Job runs in its **object's** namespace, so it crosses a namespace boundary twice: once to
push to the registry, and once to fetch its source. The chart shipped a NetworkPolicy for the push
leg and nothing for the fetch leg, and the reason turns out to matter more than the asymmetry.

The push leg is fixable from the release namespace, because the registry lives there — an ingress
policy on a pod this chart owns. The fetch leg went to source-controller in `flux-system`, which the
chart does not own and cannot write a policy for. So users hand-wrote one per build namespace, and
the failure mode until they did was `connection refused`, which reads like a broken Service.

**Shipping the missing policy would have been the wrong fix.** source-controller serves artifacts
over plain HTTP with **no authentication**, at `/gitrepository/<ns>/<name>/<sha>.tar.gz`. A pod that
can reach it can fetch *any* namespace's artifacts, not merely the one its own `ImageBuild`
references. The policy people write to make builds work is what grants every tenant's build pod a
cross-tenant source-read primitive. Making that easier and more universal would have widened the
hole while appearing to close a gap.

## Decision

**The builder serves each build its own source. Build pods never talk to source-controller.**

A per-build bearer token, in a Secret named for the Job — so the name carries the input hash, and a
token opens exactly one build. The endpoint re-resolves the `ImageBuild` and its `sourceRef` and
streams only that artifact. The URL is derived from the object every time and never taken from the
request, so the endpoint cannot be steered somewhere else.

**A pipe, not a cache.** Nothing is stored, and the pod still verifies the digest it was handed —
so a wrong answer from here is caught rather than built. That is what keeps this a transfer
mechanism rather than a second source of truth.

**Only `sourceRef`.** `context.fetch` and `context.image` are external by nature and the pod fetches
them itself. Proxying an arbitrary user URL would make the controller an SSRF amplifier, which is
precisely what `internal/netguard` and ADR 0036 exist to prevent.

The connectivity problem then solves itself the way the registry's did: the destination is a pod
this chart owns, so it ships an ingress NetworkPolicy admitting every namespace by default. Reaching
it is not reading it — the token decides that.

## Consequences

**This is a tension with [ADR 0035](0035-a-registry-is-the-only-publication-path.md), and worth
naming rather than glossing.** 0035 removed the embedded serving endpoint, finding that "two
publication surfaces was the actual cost — the fork in every path". This adds an HTTP surface back.
The distinction claimed here is that 0035 removed a **publication** surface, one users pulled
artifacts from as a product; this is a **transfer** surface to our own build pods, authenticated,
per-build, serving no one else. If that distinction ever stops being true — if anything else starts
reading this endpoint — 0035's finding applies and this should go.

**The controller is now in the data path.** A context fetch used to survive a builder restart;
it no longer does, and a builder that dies mid-stream fails that build. The fetcher's retry narrows
the window but does not remove it. Contexts stream rather than buffer, so one large build does not
become the memory ceiling for every other build at once.

**No opt-out flag, deliberately.** A `contextProxy.enabled` would reintroduce exactly the fork 0035
found expensive, and two security postures with it. The escape hatch for anyone who wants the
controller out of the data path is `spec.context.fetch` or `context.image`, which stay direct.

**`--context-base-url` unset falls back to direct fetching**, because a binary run without an
operator's configuration should still build. The startup log says plainly what that costs, and the
chart always sets it.

**What would change this decision**: source-controller growing authentication. If an artifact fetch
could carry a credential scoped to one source, the pod could fetch directly again and the proxy
would be pure cost.
