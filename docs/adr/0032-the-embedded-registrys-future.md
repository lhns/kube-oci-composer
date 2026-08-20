# 32. The embedded registry stays, and stops being the default

Date: 2026-08-20

## Status

Accepted. Revisits [0012](0012-keep-pkg-registry.md) (keep `pkg/registry`) in the light of
[0030](0030-a-real-registry-serves-both-kinds.md), which makes a real registry the serving surface
for both kinds.

No code. This record decides what happens to `internal/serve`, so that the question is settled
before anyone starts deleting or extending it.

## Context

Once a real registry is the documented path for anyone running builds, the embedded endpoint is a
second way of doing the same job. Roughly 3,000 lines maintained for a path many users will not
take.

**The case against it is not theoretical.** It has been the single largest source of defects in this
project:

- permanent per-replica tag divergence
- a replica that reported Ready while serving nothing
- `416 Requested Range Not Satisfiable` on any resumed pull
- an unauthenticated write path open to the whole cluster, while both the package documentation and
  [0025](0025-dockerfile-builds-as-a-second-kind.md) asserted it was loopback-only

Every one of those is a class of bug a real registry simply does not have, because a real registry
is a project with its own test suite and its own users.

**The case for it is also real, and it is the project's opening promise.** A small cluster that only
composes artifacts installs one component and is done. No registry to run, no credentials to
manage, no storage to back up. That property is why several of the earlier decisions look the way
they do, and it is worth something to the people it fits.

## Decision

**Keep it. Stop leading with it.**

- The embedded endpoint remains supported for `ImageComposition` in a single-replica deployment.
  That is the shape it has always actually worked in, and [0021](0021-active-standby-or-shared-storage.md)
  is the record of how much machinery the alternative costs.
- **A registry is the documented default for anyone running `ImageBuild`**, and the README says so
  plainly rather than burying it. A build's output cannot be reconstructed from its spec
  ([0025](0025-dockerfile-builds-as-a-second-kind.md)), so the store holding it is a system of
  record, and the embedded one is not built to be that.
- **No new capability is added to it.** In particular it does not gain multi-replica support, a
  pluggable backend, or a serving path for `ImageBuild`. Each of those was considered and each is
  work that a real registry has already done.

**Not deprecated.** Deprecation is a promise to remove, and there is no version at which removing
this would be the right thing for a single-node cluster that composes a config artifact and mounts
it. Saying "deprecated" and then never removing it is worse than saying what is actually meant:
supported, narrower than it was, and not where new work goes.

## Consequences

**Two supported shapes, documented as two.** The failure mode to avoid is the current one, where the
README promises "no registry to run" and the recommended path for builds quietly requires one. An
operator should be able to tell which shape they are in from the first page.

**The defect history stays relevant.** Keeping the endpoint means keeping its bugs, and the loopback
write guard is the reason the most serious of them is closed rather than open. Anything that widens
its exposure again — binding it beyond the pod, routing `/v2/` through an Ingress — reopens a hole
that took a deliberate fix to close.

**`internal/serve` is now the smaller half of the project's surface, not the centre of it.** Reviews
should weigh changes there accordingly: a defect in the registry path affects everyone running
builds, a defect in the embedded path affects single-replica composition users. That is a real
difference in blast radius and it should show up in how much scrutiny each gets.

**This record is where to argue about removal.** If the embedded endpoint accumulates further
defects, or if the bundled registry makes single-component installation cheap enough that the
distinction stops mattering, a new record supersedes this one. What should not happen is the
question being reopened informally every time someone touches the package.
