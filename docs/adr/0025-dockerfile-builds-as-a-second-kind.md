# 0025. Dockerfile builds, as a second kind with a weaker promise

## Status

Accepted, for an alpha. Amends [0016](0016-the-scope-line-is-determinism.md); it does **not**
supersede it. The scope line stays exactly where 0016 put it *for `ImageComposition`*, whose
guarantees are untouched by this record.

## Context

[0016](0016-the-scope-line-is-determinism.md) dropped an `ImageBuild` kind from the roadmap and
gave three reasons (0016:41-50). It also refused to keep it as a "someday", on the grounds that
*"a permanent 'someday' is a claim the project keeps making and never has to honour"*, and left
exactly one opening: *"re-adding an ADR later is cheap if the situation changes."*

**The situation has not demonstrably changed, and this record will not pretend otherwise.**

The motivation for revisiting was two stated needs: compile application source into an image, and
install a dependency with `apt-get`. Measured against 0016's own reasons:

1. *"It would not replace this controller, it would replace it with something weaker."* — **Answered.**
   A separate kind, binary, chart and RBAC leaves the composer's promise intact, and the weakening
   is visible at exactly one API boundary rather than smeared across a type. See *Decision*.
2. *"It is a CI system… In-cluster building earns its keep when the cluster produces the things
   being built."* — **Not answered.** Neither stated need is that situation. Compiling application
   source is what CI is for, and every user of this already has one. This is the load-bearing
   reason, and it is unmet.
3. *"The recurring need is 'take a released artifact and get it into an image', which is
   composition."* — **Largely holds.** `apt-get` is mostly served by
   [0022](0022-distro-packages-as-layer-sources.md)'s `unpack: deb`; its genuine residue is
   transitive dependency resolution and packages that only work after `postinst`, and 0022:41-45
   argues the second is the correct outcome rather than a gap. Consuming a CI-built *image* was a
   real gap, and [0024](0024-images-as-layer-sources.md) closed it.

So this ADR is written with reason 2 outstanding. The honest framing is that the alpha exists to
**produce the evidence 0016 asks for** — by being used, or by not being used — rather than to
record that the evidence already exists. If after the alpha the answer to *"is this a worse
kpack?"* is yes, 0016:52-53 already gave the disposition: *"use BuildKit or kpack directly."*

## Decision

**A second kind, `DockerBuild`, with its own controller, binary, chart and RBAC.**

Not a `build:` layer verb, and this part is settled rather than argued —
[0004](0004-two-kinds-two-controllers.md) wrote it down in advance precisely because *"the obvious
move is to add a `build:` entry type to `layers[]`. That move is wrong, and it is worth writing
down why before it looks tempting."* The mechanism: a non-deterministic entry does not weaken the
guarantee for that entry, it deletes it **for the whole object** (0004:32-34). Concretely, in this
codebase, `status.inputHash` is computed from the spec before anything is fetched and would stop
identifying the output; `publish.immutable` (default true) would stall objects nobody edited, since
identical inputs give different bytes; and the spec-hash tag pattern
([0017](0017-updating-the-consumed-digest.md):47-50) is sound *"exactly why this one cannot [`RUN`]"*.
Worst of all, `kind: ImageComposition` would stop telling a reader whether the guarantee holds —
they would have to inspect every layer of every object. That is what 0004:71-73 means by *"the
weaker guarantee silently applies to everything."*

**Idempotence is a hash of inputs recorded in status, not `output = f(spec)`.** That is exactly the
row 0004:26-28 predicted. The existing three-phase reconcile transfers structurally unchanged —
resolve from the API, hash, compare against `status.inputHash` plus one `HEAD`, and only then do
expensive work — so [0001](0001-compose-dont-build.md):54-57's objection, that *"the reconcile loop
would have to rebuild to discover whether a rebuild was needed"*, does **not** apply: every input
is resolvable from the API server without building. What does not transfer is the second
convergence check against the real output digest, because there is nothing to compare until the
build has run.

**The builder's own digest is in the input hash.** `AssemblyVersion` exists because *"a controller
upgraded to a version that assembles differently would see an unchanged input hash and keep serving
artifacts built by the old algorithm, forever"* ([0002](0002-content-addressed-inputs.md):57-60).
For a build the algorithm is not in this binary — it is BuildKit and the Dockerfile frontend — so
their pinned digests play that role. Consequences: an unpinned builder image is refused at startup,
and upgrading BuildKit rebuilds every object in the cluster. Secret *identities* are hashed
(`name` + `resourceVersion`), never values: `status.inputHash` is readable by anyone with `get`, and
a hash of a low-entropy secret is an oracle.

**Execution is one Kubernetes Job per build**, rootless, in the object's own namespace. Rejected
alternatives: a long-lived BuildKit Deployment makes the controller a confused deputy holding
credentials for every namespace and hands work to one shared daemon whose cache is a channel
between tenants; in-process BuildKit destroys `readOnlyRootFilesystem`, `drop: [ALL]`, non-root and
distroless simultaneously, which is precisely the posture 0001:56-57 named when it refused. A Job
is also an API object, so it survives leader failover and is adopted rather than restarted.

**A floating `FROM` is refused.** The direct analogue of 0002:16 applied where it can be: the
context is fetched, every `FROM` parsed, and anything not pinned by digest rejected before a Job is
created. This is the single largest source of "same commit, different image".

**The alpha pushes to a registry and does not serve.** The built-in endpoint accepts writes over
loopback only, so a Job in another pod cannot write to it. That is a clean scope cut rather than a
workaround: it keeps push credentials out of the controller, and a real registry is durable, which
sidesteps the worst consequence below.

**An `ImageComposition` consumes the result through `base.buildRef`**, resolving the digest from
`status.artifact` the way a `sourceRef` layer resolves a Flux source's — the mechanism 0004:37-40
sanctioned. The composition stays a pure function of its *resolved* inputs; what changes is what
the digest means.

## Consequences

Stated plainly, because an ADR that softened these would be worthless.

**Identical inputs may produce different bytes.** `RUN` reaches the network and the clock: package
mirror state, DNS, TLS stores, compiler-embedded build IDs, gzip level changes between BuildKit
releases. `SOURCE_DATE_EPOCH=0` with `rewrite-timestamp` narrows this and does not close it.

**Storage stops being disposable.** [0006](0006-push-is-optional.md) rests on *any lost artifact
can be rebuilt from its spec*. For a build that is false: a rebuild after storage loss may produce
a different digest than a tag already names, and with `immutable: true` that is a **permanent
terminal conflict** until a human intervenes. Retention stops meaning "how far back can I roll" and
starts meaning "how much of my only copy am I keeping". `BuildRecord` gains an `inputHash` so a
controller that lost `status.artifact` can find what it previously produced and re-verify rather
than rebuild blind.

**Provenance degrades from exact to scanned.** [0008](0008-supply-chain.md):24-30 uses this exact
kind as its counterexample — an SBOM becomes a scan of the result and provenance stops at "we ran
this Dockerfile". It also forecloses [0020](0020-is-the-supply-chain-work-worth-building.md)'s
option C, reproducibility instead of signing. 0020 is still **Open**; it should be decided before
this alpha removes one of its options.

**The spec-hash tag pattern degrades to first-writer-wins.** It identifies the *build attempt*, not
the bytes. Adequate within one cluster against a durable registry; broken across clusters, where
one commit gives two different images. Promotion between environments must be build-once,
promote-by-digest — which is ordinary CI practice, and 0016:47-48's argument arriving intact.

**`Stalled` almost never applies.** Its test is that editing *this* object's spec fixes it, but a
failing `RUN` is fixed by editing a Dockerfile in another object. Build failures take a capped
backoff with a retry ceiling instead.

**The security envelope changes shape.** Today the composer's role cannot create a single object.
A builder needs `jobs: create/delete` and turns "push to a referenced git repository" into "execute
code in that namespace". That is a genuinely different threat model, and it is the operator's call,
not the author's.

**`internal/oci` contributes nothing.** The largest, best-tested and most characteristic part of
this codebase — assembly, extraction, the archive formats — is unused by a build. That is worth
recording as evidence for 0016's first reason: this does not extend the composer, it sits beside it.

**The development loop changes for everyone.** The suite today is hermetic and fast. A builder needs
a kind cluster with rootless user namespaces in CI.

## What would settle it

The alpha is the experiment. Abandon, and say so in a superseding record, if:

- Two runs of the same context on the same builder digest do not produce the same output digest.
  Then "hash of inputs recorded in status" does less than it appears, and cold-rebuild-after-loss
  permanently breaks immutable tags.
- Rootless BuildKit does not run on the target nodes. Privileged is not an acceptable fallback —
  0001:56-57 named that blast radius as the original reason for refusing.
- The motivating workloads turn out to be expressible without `RUN`, via composition,
  [0022](0022-distro-packages-as-layer-sources.md) or [0024](0024-images-as-layer-sources.md).
- Cross-namespace isolation of cache and secrets cannot be made a flat no.
- BuildKit's release cadence cannot be tracked. Every bump moves the builder digest and rebuilds
  everything; untracked, the input hash rots into a lie.
- The honest one-line description is "a worse kpack".

## Alternatives rejected

**A `build:` layer verb.** See *Decision* and [0004](0004-two-kinds-two-controllers.md).

**One binary with a feature flag.** 0004:70-73: *"A flag set to `false` is a weaker guarantee than a
component that does not exist."* It also puts `jobs: create` in every composer install.

**Reuse the name `ImageBuild`.** A dropped identifier (0016:41). Reusing it with different semantics
makes the ADR trail unreadable. `DockerBuild` names the thing on the far side of the line rather
than a general "build" abstraction that would invite buildpacks and language toolchains.

**Keep refusing, and document harder.** The strongest alternative, and the one 0016 already chose.
It remains correct if the alpha finds no user whose need survives [0024](0024-images-as-layer-sources.md).
This record exists so that outcome is written down rather than quietly forgotten.
