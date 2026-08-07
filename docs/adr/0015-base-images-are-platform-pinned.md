# 0015. Base images are platform-pinned, and config inheritance is opt-in

## Status

Accepted.

## Context

An `image` layer entry contributes an existing image's layers, which is what turns a composition
from a bundle of files into a runnable image. Two questions had to be answered before it could be
implemented, and both have a tempting wrong answer.

## Decision

### Base layers are reused, not repacked

An image entry's layers are appended as they are. They are already content-addressed, so
unpacking and rebuilding them would change their digests for no benefit: sharing with anything
else on the same base would break, and a push would re-upload content the registry already holds.

### A multi-architecture index is refused

`go-containerregistry` will happily resolve an index to a platform, using its own defaults. That
is exactly the problem — the choice would come from the controller rather than the spec, so the
same `ImageComposition` could produce different output depending on where it ran or which version
of the library it was built against. That breaks the property everything else rests on (ADR 0002).

So a base image digest must name a platform-specific manifest. The error says how to find one:

    crane digest --platform linux/amd64 <repository>

Terminal, not retried: it needs a spec change.

#### Amended: the refusal is conditional, not absolute

Multi-architecture output arrived in [ADR 0018](0018-multi-architecture-output.md), and it changes
the scope of this rule rather than overturning it. The objection was never "an index is bad" — it
was "the CONTROLLER must not be the one choosing". With `spec.platforms` set, the choice comes from
the spec, so selecting a child per platform is exactly as spec-driven as pinning one by hand.

The rule as it now stands:

- **`spec.platforms` unset** — an index base is refused, as above, and for the same reason. The
  error additionally points at `spec.platforms` as the other way out.
- **`spec.platforms` set** — an index base is *required* shape for more than one platform. Each
  child is selected by matching the descriptor, and a platform the index does not offer is
  terminal: the spec asked for something that does not exist, and substituting a near match is how
  an amd64 binary reaches an arm node.

`TestMultiArchIndexIsRejected` and `TestIndexBaseIsAcceptedWithPlatforms` are the pair that hold
this line: neither alone says the refusal is conditional.

### Config inheritance is opt-in via `config.inherit`

The result of composing over a base is only runnable if it keeps the base's entrypoint, env, user
and working directory. Without them the image starts and immediately fails.

The tempting answer is to inherit automatically whenever a base is set. Rejected because it is
wrong for the common case: a bundle that is only ever mounted should have an empty config, and
silently acquiring an entrypoint from a base would be surprising.

So `config.inherit` is an explicit boolean. Setting it without a base is an error rather than a
silent empty config, because the symptom otherwise appears much later as a container that will not
start.

This originally named a layer entry (`config.from`), which was itself evidence that the base was
never an ordinary entry — see ADR 0016. With the base hoisted there is only one thing to inherit
from, so a boolean says everything a name did.

**The platform is inherited too.** Claiming `linux/amd64` over an `arm64` base produces an image
the kubelet refuses to run, with an error that points at the workload rather than at the
composition that caused it.

**The whole config surface is part of the input hash.** Every field lands in the image config and
therefore in the output digest, so omitting any of them would mean skipping a rebuild that was
genuinely needed. `config.from` was absent from that hash while it was unimplemented, which was a
latent bug of exactly that kind.

### Pull credentials are separate from push credentials

`layer.image.secretRef` is its own reference, not a reuse of `spec.push.secretRef`. They are
different registries with different scopes, and quietly sending a push-scoped token to whatever
registry a base image happens to live in is not a reasonable default.

## Consequences

Composing over a base couples the composition to that base's release cadence: every upstream bump
is a digest to update. That is a real cost and the reason the bundle-plus-image-volume shape
remains the default recommendation — but it is the user's trade-off to make, not the API's.

Because base layers are reused verbatim, a rebuild after changing your own content only uploads
your own layers.

## Alternatives rejected

**Resolve the index for the user.** One line, and it makes the output depend on the controller
instead of the spec.

**Inherit automatically whenever a base is set.** Convenient, and wrong for every artifact that is
only ever mounted — which is the majority of them.

**Merge the base config with explicit fields instead of replacing it.** Considered. Explicit
`labels`, `env`, `entrypoint` and `cmd` already override the inherited values, which covers the
real need; a deeper merge would raise questions with no obvious answers, such as whether env
entries are appended or replaced per key.
