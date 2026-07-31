# 0003. One ordered `layers` list, no separate `base` field

## Status

Accepted.

## Context

Most image-building APIs have a `from`/`base` field and then a list of things layered on top. It
reads naturally and it is the shape people expect.

It also creates a special case: `base` is pinned differently (often by tag), validated
differently, and the config is inherited from it implicitly. Every one of those differences is a
place where the guarantee in 0002 can quietly not apply.

## Decision

**One ordered list. `layers` is required and non-empty. There is no `base` field.**

A base image is an entry like any other:

```yaml
layers:
  - name: base
    image:
      repository: gcr.io/distroless/static
    digest: sha256:...        # same rule as every other entry
  - name: plugins
    url: https://.../core.tgz
    digest: sha256:...
    target: /plugins
```

**Layers are contributed in declaration order.** Nothing is special about position — an `image`
entry is not implicitly first, and putting one third in the list contributes its layers third.
"A base image is usually the first entry" is a convention, not a rule the API enforces.

**"scratch" is not a keyword.** It is the absence of an image entry. There is nothing to spell,
and nothing to get wrong.

**The OCI config is explicit**, via `config.from` naming an entry, rather than silently inherited
from whichever entry happens to be an image. For the artifacts that motivated this project nothing
executes the image at all, so an empty config is correct and inheritance would be surprising.

Entries are a **discriminated union from day one** — exactly one of `url`, `image`, `sourceRef`,
`configMapRef` — validated by CEL. This is the part that would be a breaking change if deferred:
hardcoding "a layer is a url plus a digest" means every later source kind alters the schema.

## Consequences

Slightly more verbose for the common case: a base image costs a three-line entry rather than a
one-line field. In exchange there is one code path, one validation rule, and one place where
ordering is decided.

## Alternatives rejected

**`base` plus `layers`.** Two concepts where one suffices, and the base inevitably acquires
exceptions — a tag instead of a digest, implicit config inheritance — that undermine 0002.

**Allow an empty `layers` list to mean scratch.** An empty list is far more likely to be a
templating accident than a deliberate empty artifact, and it would be accepted silently.

**Infer the config from the first image entry.** Convenient until a composition has two image
entries, or an image entry that is not first, at which point the rule has to be explained and
remembered. `config.from` is one line and never ambiguous.
