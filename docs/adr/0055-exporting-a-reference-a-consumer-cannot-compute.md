# 55. Exporting a reference a consumer cannot compute

## Status

Accepted, and **superseded in part by [ADR 0056](0056-the-controller-is-the-namespace-boundary.md)**.
Narrows [ADR 0017](0017-updating-the-consumed-digest.md), which rejected this by name — correctly,
for the kind it was reasoning about.

No longer true, all of it decided in 0056: the per-namespace Role (the ClusterRole carries the verbs
and the controller is the boundary); `ImageBuild` being the only kind that exports; the finalizer
being unconditional (own-namespace exports use an owner reference); and the tenant choosing the
ConfigMap's name, which is now derived.

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
- **A LABEL, not an annotation**, for the watch marker. kustomize-controller selects these with
  `--watch-configs-label-selector`, and a label selector cannot match an annotation — as an
  annotation it is inert, and inert in the way this feature is most dangerous: the ConfigMap looks
  correct and nothing rolls out. This was written as an annotation first and caught in review.
- **No watch marker by default.** `--ref-export-labels` adds none unless set; the chart shows what
  Flux wants. [ADR 0009](0009-flux-conventions-without-dependency.md) hardcodes
  `reconcile.fluxcd.io/requestedAt`, but that one is **read** — this would be **written onto an
  object in another namespace**, which is further than borrowing a convention goes.
- **Never adopt a ConfigMap this controller did not create.** `Data` is replaced wholesale, and a
  substitution source is exactly the kind of object a human writes by hand, so taking one over
  would destroy whatever else was in it. An existing ConfigMap without the managed-by label is a
  terminal spec error.
- **Labelled, not owner-referenced**, because a cross-namespace owner reference is invalid — so a
  **finalizer** deletes it instead, and only when the managed-by and owner labels both match.

## Consequences

**The write is granted per namespace, never cluster-wide.** The builder's ClusterRole stays
read-only on ConfigMaps — the boundary a test is already named for. The chart renders a Role and
RoleBinding in each namespace in `imageBuild.refExportNamespaces`, with `get`, `create` and
`update`: no `delete`, and no `list`, which would let it enumerate every substitution source in the
namespace.

The controller's own allow-list check stays. The API server stops it reaching another namespace and
the controller stops it trying; a cluster-wide grant guarded only by a flag would have made the
code the sole boundary.

**The digest becomes state outside git.** A revert commit no longer reverts the running image, and
that is the property ADR 0017's design exists to preserve. Anyone enabling this trades auditability
for the ability to consume a reference that cannot be computed in advance.

**Manifests stop being fully resolvable offline.** A substituted digest is not present in the repo,
so golden-output tests and any `grep '\${'` guard in a consuming repo need a documented exemption.
That is a reason to keep this to the few objects that need it.

**An `ImageBuild` that exports gains a finalizer**, so its deletion now depends on this controller
running. Added only when `push.writeRefTo` is set: putting one on every object would make every
deletion depend on the controller for a feature most never use.

Nothing else needs it. A build's Secrets belong to its Job and the Job belongs to the object
([ADR 0050](0050-a-builds-secrets-belong-to-the-build.md)), so those are reclaimed by Kubernetes;
the export is the one thing that cannot be.

**Nothing changes for an object that does not set it.** The field is absent by default and the
allow-list is empty, so both halves have to be turned on deliberately.
