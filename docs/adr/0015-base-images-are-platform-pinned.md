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

Multi-architecture *output* is a separate feature and remains unimplemented. When it arrives it
will mean building one composition per platform and publishing an index, with the platforms named
in the spec — not the controller guessing.

### Config inheritance is opt-in via `config.from`

The result of composing over a base is only runnable if it keeps the base's entrypoint, env, user
and working directory. Without them the image starts and immediately fails.

The tempting answer is to inherit automatically from whichever entry is an image. Rejected for the
reason in ADR 0003: it makes an image entry special, and the rule stops being explainable as soon
as a composition has two image entries or one that is not first. It is also wrong for the common
case — a bundle that is only ever mounted should have an empty config, and silently acquiring an
entrypoint from a base would be surprising.

So `config.from` names the entry to inherit from. Naming a non-image entry, or one that does not
exist, is an error rather than a silent empty config: both are typos, and the symptom otherwise
appears much later as a container that will not start.

**The platform is inherited too.** Claiming `linux/amd64` over an `arm64` base produces an image
the kubelet refuses to run, with an error that points at the workload rather than at the
composition that caused it.

**`config.from` is part of the input hash.** It selects which config is inherited, so it changes
the output. It was absent from that hash while `From` was unimplemented, which was a latent bug.

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

**Infer the config from the first image entry.** See ADR 0003; the same objection applies. Being
convenient in the two-entry case is not worth a rule that cannot be stated simply.

**Merge the base config with explicit fields instead of replacing it.** Considered. Explicit
`labels`, `env`, `entrypoint` and `cmd` already override the inherited values, which covers the
real need; a deeper merge would raise questions with no obvious answers, such as whether env
entries are appended or replaced per key.
