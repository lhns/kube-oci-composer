# 55. Exporting a reference a consumer cannot compute

## Status

Accepted. Narrows [ADR 0017](0017-updating-the-consumed-digest.md), which rejected this by name —
correctly, for the kind it was reasoning about.

## Context

ADR 0017 chose spec-hash tags so a consumer can compute the reference it will pull **without
reading status**: hash the build-determining part of the spec, write the result into both
`push.tags` and the workload's image reference, and one commit moves the artifact and its consumer
together. It rejected a ConfigMap indirection because the problem *"disappears"* under that design.

That reasoning holds for `ImageComposition`, where `output = f(spec)`. It is weaker for
`ImageBuild`, whose own CRD concedes the digest *"is NOT a function of its spec — it is an
observation"* ([ADR 0025](0025-dockerfile-builds-as-a-second-kind.md)). A consumer cannot compute
what a build will produce, so the tag is the only handle — and a tag is a name that can be made to
mean something else.

[ADR 0054](0054-name-it-after-you-push-it.md) made `onConflict` exact by uploading before naming.
It also made something else available: **if nothing is tagged, nothing can be remeaned.** A build
that publishes by digest and exports the result has no tag, so the conflict question does not
arise — it becomes inapplicable rather than approximate.

### The case against, which is most of the design

- **A ConfigMap change does not trigger a reconcile by default.** Only with
  `reconcile.fluxcd.io/watch: Enabled`, or a kustomize-controller started with
  `--watch-configs-label-selector`. Absent that, substitution is picked up at the next interval and
  nothing says why.
- **A missing key substitutes the empty string, silently.** A missing *object* fails the
  reconciliation; a missing *key* does not, and there is no server-side strict mode.
- **The existing design fails loudly; this one fails quietly.** A spec-hash tag that does not move
  when it should is caught by `onConflict`. A digest arriving through a ConfigMap converts that
  class of failure into a silent auto-update.
- **Flux's own `ImagePolicy`/`ImageUpdateAutomation` answers this**, and its decisive advantage is
  that it writes the value **back into git**.

## Decision

**`push.writeRefTo`, strictly opt-in, refused unless the operator allow-lists the target
namespace.**

```yaml
push:
  writeRefTo:
    name: pymods-ref
    namespace: flux-system
    keys:
      ref: PYMODS_REF          # registry/repo@sha256:...
      digest: PYMODS_DIGEST    # sha256:...
```

Each requirement below follows from a failure mode above, and none is optional:

- **The full ref, not only the digest.** A consumer substituting a bare digest produces a trailing
  `@` on an empty string if the key is ever absent — which fails later and less legibly than a
  missing image would.
- **The controller sets the watch annotation.** Without it the feature does not do the thing it
  exists to do, and its absence is invisible.
- **All keys or none, and only after a confirmed publish.** The ConfigMap is replaced wholesale
  rather than patched key by key, so a consumer never sees one field updated and another stale. An
  incomplete reference is refused rather than written.
- **An allow-list, empty by default.** `--ref-export-namespaces`. The useful target is the
  consuming Kustomization's namespace — usually `flux-system`, which parameterises everything — so
  this is a real privilege escalation and the default must permit nothing.
- **Labelled, not owner-referenced.** A cross-namespace owner reference is invalid, and the useful
  target is another namespace. So the ConfigMap is **not** garbage-collected with the object.

## Consequences

**The RBAC grant is conditional, and that took a second look.** The builder's ClusterRole was
read-only on ConfigMaps by a deliberate boundary — *everything this controller writes into a tenant
namespace is a Secret* — guarded by a test named for it. The chart therefore grants `create` and
`update` **only when `imageBuild.refExportNamespaces` is set**: the feature and the privilege are
turned on by the same value, and a default install still ships a controller that cannot write a
ConfigMap anywhere. The generated role carries the verbs unconditionally, so the chart/role drift
test renders with the feature on and the default render is asserted separately.

**The digest becomes state outside git.** A revert commit no longer reverts the running image, and
that is the property ADR 0017's design exists to preserve. Anyone enabling this trades auditability
for the ability to consume a reference that cannot be computed in advance.

**Manifests stop being fully resolvable offline.** A substituted digest is not present in the repo,
so golden-output tests and any `grep '\${'` guard in a consuming repo need a documented exemption.
That is a reason to keep this to the few objects that need it.

**The exported ConfigMap outlives its object.** Deleting the `ImageBuild` leaves it behind, to be
cleaned up by whatever manages that namespace.

**Nothing changes for an object that does not set it.** The field is absent by default and the
allow-list is empty, so both halves have to be turned on deliberately.
