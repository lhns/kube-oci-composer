# 0004. Two kinds, two controllers, two charts, one repo

## Status

Accepted. `ImageBuild` is not implemented and may never be; this record exists so that v0.1 does
not make it impossible.

## Context

Composition cannot run a Dockerfile (0001). Sooner or later someone will want one, and the obvious
move is to add a `build:` entry type to `layers[]`.

That move is wrong, and it is worth writing down why before it looks tempting.

## Decision

If Dockerfile support ever ships it is a **separate kind**, `ImageBuild`, with its own controller,
its own chart, and its own weaker promise. It is never a layer source.

| | `ImageComposition` | `ImageBuild` (hypothetical) |
|---|---|---|
| inputs | content-addressed artifacts | Dockerfile plus context |
| idempotence | output digest = f(spec) | hash of inputs recorded in status |
| reproducible | yes, bit-for-bit | no — `RUN` is not deterministic |
| privileges | none, in-process | rootless BuildKit pod |
| scope | small | a CI system |

**Why not a `build:` layer entry.** One non-deterministic entry makes the entire composition
non-deterministic. The reconcile loop, the provenance claims and the input-hash short-circuit all
stop being true — not for the build entry, for the whole object. Keeping the promise per-kind
keeps it honest.

They still compose, through the mechanism Flux uses everywhere: a layer entry may reference
another object's resolved artifact, the way `Kustomization.sourceRef` consumes a `GitRepository`'s
`status.artifact`. Determinism is then "output = f(resolved input digests)" — the same guarantee
Flux gives, not a weaker one.

**Two deployments, not one binary with a flag.** The composer needs no privileges and runs
everything in-process; a builder would spawn BuildKit pods and need RBAC to do it. Bundling them
raises the composer's blast radius to the builder's for no benefit, and you could no longer
install one without the other. This is also what Flux itself does: source, kustomize, helm,
notification, image-reflector and image-automation are six deployments.

**One repository**, because they share the API group, the `Push`/`ArtifactStatus` types, and the
registry and assembly code.

### What v0.1 had to get right

1. `layers[]` is a discriminated union from day one (0003).
2. `Push` is a standalone shared struct, not inlined per kind.
3. `status.artifact{revision,digest,ref}` is identical across kinds, so anything downstream can
   consume either without caring which produced it.
4. Two `cmd/` entry points are anticipated in the layout.

## Consequences

The repository is named for the composer but would house a builder. Accepted deliberately: better
a slightly wide name than one chosen for a controller that may never exist. If it ever becomes
genuinely confusing, `ImageBuild` moves to its own repository rather than renaming the published
chart and artifacts of a working component.

## Alternatives rejected

**One kind with a `build:` source.** Covered above: the weaker guarantee silently applies to
everything.

**One binary, two feature flags.** A flag set to `false` is a weaker guarantee than a component
that does not exist. It also means every composer deployment carries the code and the RBAC surface
of a builder.

**Separate repositories now.** Premature. They share types and code, and a second repository for a
kind that does not exist yet is pure overhead.
