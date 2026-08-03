# 0016. The scope line is determinism, not Dockerfile parity

## Status

Accepted. Supersedes [0004](0004-two-kinds-two-controllers.md) entirely, and the part of
[0003](0003-one-ordered-layers-list.md) that treats a base image as an ordinary layer entry.

## Context

After the first working version, the honest description of the project was "a composer that
cannot do most of what a Dockerfile does". Listing what was missing seemed to confirm it:

- delete a file inherited from a base
- set `user`, `workingDir`, `exposedPorts`, `volumes`, `stopSignal`
- control file mode and ownership
- select a subdirectory from an archive
- produce a multi-architecture index

That framing invited the obvious question: if it is going to grow toward a Dockerfile anyway, why
not implement `ImageBuild` (0004) and be done?

The framing was wrong. **Every item on that list is a pure function of its inputs.** None needs to
execute anything. They were not out of scope; they were unimplemented. The project was not
half-baked in *scope*, it was half-baked in *coverage*.

## Decision

**The line is determinism. `RUN` is the only thing on the far side of it.**

In scope — anything where the output digest is a function of digest-pinned inputs:

- placing, removing, re-owning and re-permissioning files
- the entire OCI config surface
- reusing a base image's layers
- multi-architecture output, where the platform list comes from the spec

Out of scope, permanently: executing anything. That is what makes the output digest unknowable
without building, and it is what would force a BuildKit pod, RBAC to spawn it, a build cache, and
the loss of the cheap reconcile.

**`ImageBuild` is dropped from the roadmap.** Not because building images is bad, but because:

1. It would not *replace* this controller, it would replace it with something weaker. 0004 already
   sets out why the guarantees cannot be mixed, and that argument holds against making the builder
   primary just as well as against a `build:` layer entry.
2. It is a CI system. Building software from source is well served by existing CI, which every
   user of this already has. In-cluster building earns its keep when the cluster produces the
   things being built, which is a different situation from the one that motivated this project.
3. The recurring need in a GitOps cluster is not "compile this", it is "take a released artifact
   and get it into an image". That is composition.

If in-cluster Dockerfile builds are ever genuinely needed, the honest answer is to use BuildKit or
kpack directly rather than to write a third one.

## Consequences

**The repository name stops being a wart.** 0004 accepted "if `ImageBuild` ever ships it will live
in a repo called composer" as a known cost. It never will, so the name is exactly right.

**"Missing feature" now has a test.** Anything proposed for this API has to be a pure function of
its inputs. That is a sharper question than "does Docker have it", and it answers itself.

**The base image is hoisted out of the layer list**, which reverses 0003. Its premise was that an
image entry is an ordinary entry, and implementation produced three exceptions:

- the config had to name which entry was the base (`config.from`), so the API already knew it was
  special;
- an image entry contributes many layers where every other entry contributes exactly one;
- multi-architecture resolves the base per platform and nothing else.

Three exceptions is enough to conclude the abstraction was wrong. With `base` hoisted, "each entry
produces exactly one layer" becomes true, and `config.from` collapses into `config.inherit`.

What survives from 0003 is the part that was right: ordering is declaration order, nothing is
implicitly first among the layers, and "scratch" is the absence of a base rather than a keyword.

**`remove` becomes a fourth verb** alongside `fetch`, `configMap` and `sourceRef`. It is an
ordered filesystem operation, so it belongs in the ordered list; it produces a whiteout-only layer.
It takes no `to`, `owner` or `mode`, which one CEL rule enforces.

## Alternatives rejected

**Freeze the composer where it was.** Least work, and it would have left a tool that cannot set a
user or remove a file for no principled reason — gaps that are hard to explain because there is no
principle behind them.

**Implement `ImageBuild` and make it primary.** Covered above. It trades the properties that
justify this project's existence for capabilities that already exist elsewhere.

**Keep `ImageBuild` on the roadmap as a someday.** A permanent "someday" is a claim the project
keeps making and never has to honour. Deleting it is more honest, and re-adding an ADR later is
cheap if the situation changes.
