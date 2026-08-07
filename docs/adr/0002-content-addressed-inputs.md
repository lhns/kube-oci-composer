# 0002. Every input is content-addressed; the output digest is a pure function of the spec

## Status

Accepted. Widened once, deliberately: see *Declared versus resolved* below.

## Context

0001 rests on the output digest being derivable from the spec. That holds only if every input is
pinned. One unpinned input — a mutable tag, a URL whose contents can change — and the output is
no longer a function of anything, which breaks idempotence, makes provenance a guess, and turns
every reconcile into a rebuild.

## Decision

**Every layer entry is content-addressed. There are no exceptions anywhere in the API.**

Assembly is normalised so that identical inputs give byte-identical output: entries sorted, a
fixed epoch timestamp on every tar header, uid/gid zeroed, modes reduced to 0755 or 0644, and the
config's `History` dropped because it carries timestamps. `TestAssembleIsDeterministic` asserts
this directly, and it is the load-bearing test of the project.

A digest that does not match the fetched bytes is **terminal**, not a retry. Retrying cannot make
wrong bytes right, and a silent retry loop would hide tampering rather than surface it.

### Declared versus resolved

Originally every digest was written by hand in the spec. Two later sources have no hand-written
digest and should not need one:

- a Flux `GitRepository` (or `OCIRepository`, `Bucket`), whose `status.artifact.digest` is
  published by source-controller;
- a `ConfigMap`, whose content the controller can hash itself.

Requiring a human to paste a digest for content the cluster already addresses would be
ceremony, not safety. So the rule is stated more precisely:

> Every input is content-addressed **at build time**. The digest may be *declared* in the spec,
> or *resolved* by the controller from a source that is itself content-addressed.

The guarantee is unchanged — output is a pure function of *resolved* inputs — and it is exactly
what Flux already does when a `Kustomization` consumes a `GitRepository`. What changes is only
who writes the digest down.

### The input hash

The output digest is a pure function of the spec, but learning it used to require downloading
every layer and assembling them. On an hourly interval that meant re-pulling tens of megabytes
forever to discover nothing had changed.

`status.inputHash` is a hash over the ordered `(layer digest, unpack, target)` tuples, the config,
and an `AssemblyVersion` constant. When it is unchanged and the published artifact still resolves
to the recorded digest, the reconcile costs one `HEAD`.

Three details matter:

- **`AssemblyVersion` is in the hash.** Without it, a controller upgraded to a version that
  assembles differently would see an unchanged input hash and keep serving artifacts built by the
  old algorithm, forever.
- **Name, URL and local path are excluded.** A rename or a switch to a mirror serves identical
  bytes by definition; rebuilding for either is precisely the cost this removes.
- **Fields are length-prefixed.** Concatenation would let a rearrangement across a field boundary
  collide, and a real change would then be silently skipped.

This is **not** the same idea as the `inputHash` sketched for `ImageBuild` (0004). There the hash
*is* the identity, because a Dockerfile's output digest is unknowable without building. Here the
output digest remains the identity and the hash is only a short-circuit — the convergence check
against the real digest is still performed after assembly.

### The one input that does not come from the spec

Multi-architecture output (ADR 0018) added `spec.platforms`, and with it a default: when the list
is unset and there is **no base**, the artifact is built for the *controller's own* architecture.
That is an environment input, and it is the only one in the API. It is recorded here rather than
left implicit, because "a pure function of the spec" is the sentence everything else leans on, and
a quiet exception to it is worse than a stated one.

Why it is worth taking:

- On a single-architecture cluster it is exactly the value that was previously **hardcoded**
  (`linux/amd64`), so nothing changes. `TestUnsetPlatformMatchesTheOldHardcodedDefault` pins that.
  Without it, upgrading the controller would republish every artifact under spec-hash tags that had
  not moved, which `immutable: true` turns into a failed build across the estate at once.
- On an arm64-only cluster it starts being *correct*, where the hardcoded value produced artifacts
  the kubelet refuses.
- The alternative — requiring `platforms` on every base-less artifact — is ceremony for the common
  case, which is a bundle of platform-neutral files.

What it costs, plainly: on a **mixed-architecture** cluster the same spec can produce different
content depending on which node the leader is on, and immutable tags then fail the build. The fixes
are to name `platforms` explicitly, or to pin the controller to one architecture with a
`nodeSelector`.

The OS is **not** taken from the runtime — it is always `linux`. `runtime.GOOS` describes the
machine the controller binary was built for, so a controller built on Windows or macOS would stamp
an artifact nothing can mount, from a spec that never mentioned Windows. Only the architecture is
genuinely in question. (This was caught by a test, not by review.)

Everything derived from a **base** remains a pure function of the spec: the base digest is pinned,
so the base's platform is too. Adding `platforms` also closed a real hole here — the base digest
reached the output but **not** the input hash, so repointing `spec.base.digest` left the hash
unchanged, the short-circuit engaged, and the new base was silently never built.

## Consequences

Upgrading a dependency means updating a digest, which is more friction than editing a version
string — and is the point. `TestInputHashIsPinned` pins the hash against accidental change,
because bumping it rebuilds every artifact in every cluster.

## Alternatives rejected

**Allow tags for convenience.** One mutable input makes the whole composition non-deterministic,
and the weaker guarantee then silently applies to everything.

**Verify lazily, or trust the source.** Verification is the only thing that makes a mismatch
visible. Streaming the hash during download costs nothing on top of the transfer.

**Re-verify cached content on every read.** Content is verified when written. Re-hashing on each
reconcile reintroduces a smaller version of the cost the cache exists to remove, for no additional
guarantee. Content arriving from the *shared remote* cache tier is verified, because that tier may
hold bytes this process never checked.
