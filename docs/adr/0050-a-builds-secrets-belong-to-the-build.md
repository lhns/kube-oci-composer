# 50. A build's Secrets belong to the build

## Status

Accepted.

## Context

A ten-day-old 0.5.0 install, cross-referencing Secrets against `status.history`:

```
jetkvm-ui:         11 secret sets,  2 in history  ->  9 orphaned
jetkvm-cloud-api:  10 secret sets,  5 in history  ->  5 orphaned
```

**42 of 63 Secrets in that namespace were garbage** — by far the largest Secret consumer in the
cluster, ahead of a three-node Postgres. Namespaces using only `ImageComposition` had none, because
only `ImageBuild` creates them.

`startBuild` creates up to four per build: `-push`, `-registry-ca`, `-dockerfile`, `-context`. The
report found three; `-registry-ca` exists only with TLS enabled.

### The leak needs two things

1. **The owner is the `ImageBuild`.** Kubernetes reclaims a dependent only when its owner is
   deleted, and a GitOps-managed object is not. The `Job` — which *does* go, via
   `TTLSecondsAfterFinished: 3600` or the explicit delete on retry — owned nothing.
2. **The names change per build.** Each is `jobName(obj, inputHash)` plus a suffix, so a new input
   hash produces a new set beside the old rather than updating it.

Neither alone leaks: with only (1) each build would overwrite the same four; with only (2) the Job's
deletion would sweep each generation. Together they leak one set per revision, forever.

Nothing compensated. The only `Delete` in `internal/buildcontroller` is for Jobs, and the builder
holds `get;create;update` on Secrets — **no `delete`** — so as shipped it could not have cleaned them
up even if it had tried. Each carried an annotation reading *"Deleted with the build."*

The GitOps layer cannot help either: the Secrets carry no `kustomize.toolkit.fluxcd.io` labels and
never appear in any inventory, because their names embed a controller-computed hash. Only the
controller can know which are dead.

## Decision

**The Job owns them.** Immediately after `r.Create(ctx, job)` succeeds, each Secret's controller
reference is replaced with one to the Job.

That is the whole fix, and it removes cause (1) so that cause (2) stops mattering — each generation
is attached to its own Job and goes when that Job does. Kubernetes performs the deletion, so there
is no pruning loop, nothing to reconstruct, and no way to delete a running build's credentials,
because the Job that owns them exists for exactly as long as it needs them.

**Created owned by the ImageBuild, then re-owned.** An owner reference needs an owner that already
exists, and the Job does not until its Secrets are named in its pod spec.

**Not a list-and-prune sweep.** That would need `list` on Secrets cluster-wide — the power to
enumerate and read every Secret in every namespace — where this needs only `update`, which the
builder already has. **No RBAC change.**

**Both owners cannot be kept.** A dependent is reclaimed only when *all* its owners are gone, so
leaving the ImageBuild reference alongside the Job's would preserve the bug exactly.

### Three guards, because one of these names is not ours

`pushSecretFor` returns **the object's own Secret** when `spec.push.secretRef` is set — one this
controller neither created nor owns. Adopting whatever name it returned would hand a user's
credential to the Job's garbage collection and delete it an hour after the build finished.

So adoption never uses the returned names. It derives the four it would have created, and takes a
Secret only if all three hold: the name is one this controller generates, the
`app.kubernetes.io/managed-by` label is one it sets, and it is currently controlled by *this*
object. A user's Secret fails at least two.

**A failed re-owning is logged, not fatal.** The Job is already running; the build must not fail
over its own housekeeping, and the outcome is the previous behaviour — a leak, not an outage.

## Consequences

**Existing orphans are not cleaned up.** Their owner is the `ImageBuild` and nothing re-parents them
retroactively. This stops the accumulation; the backlog needs a manual sweep.

**A crash between creating the Secrets and re-owning them leaves one build's worth** owned by the
ImageBuild. Bounded, and those names are reused if that same input hash builds again.

**Deleting a Job now deletes its credentials.** That is the point, but it means a Job cannot be
re-run by hand after its Secrets have gone — the supported path is to let the controller start a new
build, which creates them fresh.

**The annotation is now true.** It said *"Deleted with the build"* while nothing deleted them; it
now says which build, and Kubernetes enforces it.
