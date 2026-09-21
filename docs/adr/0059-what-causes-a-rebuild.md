# 59. What causes a rebuild

## Status

Accepted. Records behaviour that has been true since [0002](0002-content-addressed-inputs.md) and
written down nowhere as a decision — only as side effects of records about other things
([0025](0025-dockerfile-builds-as-a-second-kind.md), [0051](0051-a-build-that-lost-its-image-rebuilds-it.md))
and as doc comments on `oci.AssemblyVersion` and `build.Inputs`.

## Context

People plan upgrades around this. They ask whether a restart rebuilds, whether `spec.interval`
paces rebuilds, whether bumping the chart moves digests. The answers are all determined by four
lines of code — the short-circuit in each controller — and a reader had to find those lines to
learn them. An undocumented rule that users depend on is a rule that gets broken by accident.

## Decision

**A rebuild happens exactly when the input hash changes, or when what was published is no longer
in the registry.** Nothing else causes one: not the reconcile interval, not a controller restart,
not leader failover, not the passage of time, not the `reconcile.fluxcd.io/requestedAt`
annotation. Those decide *when the question is asked*; the two conditions above are the only
answers that start work. `spec.interval` (default 1h, `recon.Interval`) is a re-**check** cadence.

**The hash covers everything that determines the output, including the toolchain.** For an
`ImageComposition`: the ordered layer digests with their unpack modes and targets, the config, the
base pin, the declared platforms, and `AssemblyVersion`. For an `ImageBuild`: the context kind,
digest, subpath, strip and unpack mode, the Dockerfile form and content, target, network, cache
mode and ref, attestations, `SOURCE_DATE_EPOCH` policy, platforms, build args, secret *identities*
(`name`/`resourceVersion`, never values), `RecipeVersion`, and the pinned builder, frontend and
fetcher digests.

**The toolchain-ish inputs are in the hash deliberately**, and they are the reason an upgrade is
never a no-op. `AssemblyVersion` and `RecipeVersion` exist so an upgraded controller that assembles
or invokes BuildKit differently cannot look at an unchanged hash and keep serving output from the
old algorithm ([0002](0002-content-addressed-inputs.md):57-60). The builder, frontend and fetcher
digests play that role for the parts that are not in this binary, and
[0057](0057-the-toolchain-is-an-input.md) extends it to the Go compiler.

**A few things are excluded on purpose.** `ContextRevision` is recorded but not hashed: the context
digest already identifies the content, and hashing both would rebuild on a no-op repack. Layer
names and on-disk paths are excluded for the same reason. Secret values are excluded because
`status.inputHash` is readable by anyone with `get`, and a hash of a low-entropy secret is an
oracle.

**The two kinds ask "is it still published?" differently, and that asymmetry is the one in
[0025](0025-dockerfile-builds-as-a-second-kind.md).** A composition's output is a function of its
spec, so a rebuild reproduces the same digest; it can therefore treat *any* unresolved `HEAD` as a
reason to rebuild, and it checks the recorded digest, every requested tag, and whether the
attestations it recorded are still complete for that digest. An `ImageBuild`'s output is an
observation, so a rebuild may produce different bytes; it therefore rebuilds only on a definite
404, because anything weaker turns a registry outage into a cluster-wide build storm
([0051](0051-a-build-that-lost-its-image-rebuilds-it.md)).

## Consequences

**Upgrading the operator rebuilds every object**, whenever the upgrade moves `AssemblyVersion`,
`RecipeVersion`, a pinned builder/frontend/fetcher digest, or the Go toolchain. This is the
intended behaviour and not an incident. It is also why those bumps are release-note material.

**Changing nothing rebuilds nothing, forever.** An object whose spec and resolved inputs are stable
will converge once and then cost one `HEAD` per interval indefinitely, across any number of
restarts.

**The supported way to force a rebuild is to change an input.** Move a dependency tag, repoint the
base, edit the Dockerfile — a comment is enough. There is no force flag and this record does not
propose one; the reconcile annotation re-asks the question and will answer "nothing to do".

**Losing `status` means rebuilding.** `status.inputHash` and `status.artifact` are the only record
linking a set of inputs to an output. For a composition that is cheap — the same bytes come back.
For an `ImageBuild` it is not: the output is unrecoverable, and the rebuild replaces it under the
same name with a different digest. `BuildRecord.inputHash` was added so a controller *could* find
what it previously produced instead; the read path is still not implemented
([0025](0025-dockerfile-builds-as-a-second-kind.md)), so today a lost status is a rebuild.
