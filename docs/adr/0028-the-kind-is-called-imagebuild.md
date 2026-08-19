# 28. The kind is called ImageBuild

Date: 2026-08-19

## Status

Accepted. Reverses the naming decision in
[0025](0025-dockerfile-builds-as-a-second-kind.md):163-166.

## Context

0025 named the kind `DockerBuild` and rejected `ImageBuild` in as many words:

> **Reuse the name `ImageBuild`.** A dropped identifier (0016:41). Reusing it with different
> semantics makes the ADR trail unreadable. `DockerBuild` names the thing on the far side of the
> line rather than a general "build" abstraction that would invite buildpacks and language
> toolchains.

Both halves of that have worn badly.

**The identifier was dropped, not spent.** [0016](0016-the-scope-line-is-determinism.md):41 removed
`ImageBuild` from the roadmap because the kind was not going to be built. 0025 then built it. An
identifier reserved against a decision that has since been reversed is not a reason to pick a worse
name — and the trail stays readable precisely because these records exist and say so.

**`DockerBuild` is inaccurate.** Nothing in the implementation uses Docker. The build runs rootless
BuildKit (`internal/buildcontroller/job.go`), driven by `buildctl`, with the Dockerfile frontend
pinned by digest. What the kind consumes is a *Dockerfile*; what it runs is BuildKit. 0025's own
planning notes conceded the point and chose the name for readability rather than accuracy.

The worry about inviting buildpacks and language toolchains is real but is not carried by the name.
It is carried by [0016](0016-the-scope-line-is-determinism.md)'s scope line and by this kind's own
API surface, which takes a Dockerfile path and nothing else.

## Decision

The kind is `ImageBuild`. `dockerbuilds.oci.lhns.de` becomes `imagebuilds.oci.lhns.de`, the short
name `dbuild` becomes `ibuild`, and the Go types follow.

**The component keeps its name.** The binary is still `oci-builder`, the chart still
`kube-oci-builder`, the image still `kube-oci-builder`. None of them ever contained "docker" — they
are *builder*-flavoured — so renaming them would be churn without meaning.

**A clean break, not a migration.** Kubernetes cannot rename a CRD's `spec.names`, so
`imagebuilds.oci.lhns.de` is a different resource. There is no conversion webhook and only one API
version, so nothing can carry objects across.

## Consequences

**Existing objects are lost, and this is a breaking change.** An upgrader must
`kubectl get dockerbuilds -A -o yaml`, rewrite `kind:`, re-apply, and delete the old CRD — which
deletes anything left under it. Helm never upgrades or deletes `crds/`, so a chart upgrade leaves the
old CRD in place and adds the new one; the old must go by hand.

**Status is lost with them, so every object rebuilds once.** That is harmless *today* only because
`push.immutable` is inert on this kind — the field is in the CRD and nothing reads it, so a rebuild
producing different bytes silently overwrites the tag. The moment that field is made real, a
rebuild after status loss becomes a terminal conflict (0025:105-112). **The rename must therefore
land before the tag policy does**, and this record exists partly to fix that order.

**The alpha designation is what makes this affordable**, and it is the last point at which it is.
0025 shipped the kind as an alpha whose purpose is to produce evidence; renaming a stable kind would
not be defensible on these grounds.

**The ADR trail now names two kinds for one thing.** 0025, 0026 and 0027 say `DockerBuild` and are
left untouched, because a record is evidence of what was believed when it was written. This record
is the bridge. That is the cost 0025 was trying to avoid, and it is smaller than carrying an
inaccurate name for the life of the API.
