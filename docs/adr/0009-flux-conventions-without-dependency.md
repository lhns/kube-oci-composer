# 0009. Flux conventions without a Flux dependency, and the name

## Status

Accepted.

## Context

This controller fills a gap in the Flux toolkit: `source-controller` consumes external sources and
produces artifacts, but nothing in the toolkit produces an OCI **image**. That is a symmetric gap
in an otherwise symmetric design, and it makes the project look like a Flux component.

It is not one. Given URLs and digests it assembles an artifact and publishes it, on any cluster,
with or without Flux.

## Decision

**Borrow the conventions, not the dependency.**

Implemented, and asserted by tests:

- **kstatus conditions**: `Ready`, `Reconciling`, `Stalled`. `Stalled` means a *terminal* error —
  a digest mismatch, an invalid spec — and is **not retried**; transient failures stay
  `Reconciling` and are returned so controller-runtime backs off. Getting this split right is most
  of what makes a controller feel Flux-like, and it is easy to get subtly wrong.
- **`reconcile.fluxcd.io/requestedAt`** plus `status.lastHandledReconcileAt`, so `flux reconcile`
  works out of the box.
- **`spec.suspend`** halts reconciliation without deleting anything.
- **`spec.interval`** as the resync period; **`status.observedGeneration`** so `kubectl wait
  --for=condition=Ready` is meaningful.
- **`status.artifact{revision,digest,ref}`**, **`secretRef`**, events through the standard
  recorder for `notification-controller`, and finalizers.

**No `fluxcd/pkg/runtime` dependency.** The conventions are a wire contract, not a library, and
taking the dependency would tie this controller's release cadence to Flux's for a handful of
helpers that `apimachinery` already provides.

### The name

**"OCI", not "image".** The scope is OCI artifacts; a container image is one media type among
them. Flux uses the same vocabulary — it has a kind called `OCIRepository` and `flux push artifact`
publishes non-image artifacts. It collides with Oracle Cloud Infrastructure in search results,
which is a search annoyance rather than a comprehension problem next to a `kube-` prefix.

**No `flux-` prefix.** It would claim a dependency that does not exist and read as endorsed by the
Flux project, inviting trademark friction. What is borrowed is conventions, which is a README
sentence rather than a name.

**`kube-` prefix**, matching the sibling operators, which is what makes the family legible.

## Consequences

Flux compatibility is asserted by our own tests rather than guaranteed by a shared library, so a
convention change upstream would be noticed late. The conventions in question are stable and
widely implemented; the trade is worth it.

Composition with the rest of the toolkit is deliberate and one-way: the composer guarantees the
artifact **exists**; `image-reflector` notices it, `image-automation` commits the digest to git,
and a normal `Kustomization` deploys it. Git stays the source of truth for what is *deployed*; the
controller owns what is *available*. That division is why this fits (0010).

## Alternatives rejected

**Depend on `fluxcd/pkg/runtime`.** Real convenience for condition and patch helpers, at the price
of a version constraint on a project we do not otherwise depend on, in a controller that must run
without Flux.

**Invent our own status vocabulary.** Anyone deploying this already runs Flux controllers and
already knows what `Stalled` means. A novel vocabulary would be a private dialect for no gain.

**Propose it upstream as a Flux controller.** Perhaps one day. It would be a poor first move:
building was excluded on purpose (0001), and arriving with a working, independently useful
controller is a better argument than a proposal.
