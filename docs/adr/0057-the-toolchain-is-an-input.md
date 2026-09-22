# 57. The toolchain is an input to the artifact

## Status

Accepted. Records a property of [ADR 0002](0002-content-addressed-inputs.md)'s `AssemblyVersion` that was
always true and never written down, plus a zot behaviour the chart had wrong.

**Corrected before release: the `AssemblyVersion` 2 → 3 bump below is withdrawn.** The principle
stands; the premise did not. The release image had been built with Go 1.27 since v0.5.0 (the
`Dockerfile` said `golang:1.27`); only `go.mod`, and so CI, was on 1.26. So production digests never
moved with this change, and the bump alone would have changed every composition's input hash on
upgrade -- and, under a spec-hash tag with `onConflict: Fail`, stalled every one of them
permanently, since a terminal conflict is not retried. The toolchain that counts is the one the
release image is built with, so CI now fails if the `Dockerfile`'s Go minor differs from `go.mod`'s,
and the `Dockerfile` pins its image by digest.

## Context

Two things found on the same afternoon, from opposite directions, that turn out to be the same
mistake: **a rule that reads as protection and is not one.**

### A Go release changes every digest

Dependabot opened a routine bump: four libraries and `go 1.26.0` → `1.27.0`. CI went red and the
cause was not any of the libraries.

Isolated by reverting one thing at a time, with the four library bumps kept throughout:

| change | result |
|---|---|
| revert go-containerregistry | still fails |
| also revert klauspost/compress | still fails |
| keep all four bumps, revert **only** `go 1.27.0` → `1.26.0` | **passes** |

The uncompressed layer was identical — same `diff_id`, same config digest — and the compressed layer
went from 202 to 204 bytes. Go 1.27 changes `compress/flate`'s output for the same input. Nothing
about the spec changed, and nothing about this project's code changed.

**That is a wedge, not a nuisance.** The recommended pattern is a tag derived from a hash of the
spec ([ADR 0017](0017-updating-the-consumed-digest.md)). An upgraded controller recomputes the same
spec hash, publishes to the same tag, and produces different bytes — so `onConflict: Fail`, the
default, refuses it. Every `ImageComposition` in a cluster stalls at once, on an upgrade that
mentioned four libraries.

`AssemblyVersion` exists for exactly this and its doc comment lists *"entry ordering, header
normalisation, media types, the config it stamps"* — all things in **this** repository. The
compressor is not, and nothing said it counted.

### zot's `keepTags` was configured to do something it never did

The chart rendered two retention entries and a comment claiming *"a tag survives if EITHER keeps
it"*:

```jsonc
"keepTags": [
  { "patterns": [".*"], "pulledWithin": "720h" },
  { "patterns": [".*"], "pushedWithin": "720h" }
]
```

`getTagPolicy` (v2.1.21) returns on the first pattern that matches:

```go
for idx, tagPolicy := range tagPolicies {
    if p.regex.MatchesListOfRegex(tag, tagPolicy.Patterns) {
        return tagPolicy, idx, nil
    }
}
```

Both entries match `.*`, so the second was **never evaluated**. Only `pulledWithin` was ever in
force. Rules *within* one entry are OR-ed — *"we retain candidates if any of the below rules are
met"* — so the configuration that does what two entries appeared to do is one entry carrying both.

The practical cost: **a tag pushed and never pulled was protected by nothing.** That is precisely a
freshly published spec-hash tag, in the window between publishing and the refresher first reaching
it. It is a candidate explanation for the incident behind the retention work — tags vanishing
minutes after a successful push — and it means
[ADR 0053](0053-a-publish-is-protected-before-the-reconcile-returns.md)'s refresh-at-publish was
holding a line nobody knew was undefended.

## Decision

**The toolchain is part of what determines the artifact, and a toolchain change is a migration.**

- Upgrading the Go minor version is not a dependency bump. It goes in its own commit, with
  `AssemblyVersion` bumped so spec-hash tags move deliberately rather than colliding, and a
  CHANGELOG entry saying what operators will see.
- `AssemblyVersion`'s doc comment says so, so the next person reading "bump this whenever Assemble's
  output changes" knows the compressor counts.
- This generalises past Go. Any step that **recompresses** may change bytes without changing
  content: a different zstd level, a gzip implementation, a BuildKit release. `BuilderDigest`
  already covers the build tool for `ImageBuild`; this is the composer's equivalent.

**`keepTags` is one entry carrying every rule, never a list of single-rule entries.** Enforced by a
test that asserts the count, because the previous test asked only whether *some* entry carried each
rule — which both shapes satisfy, and which is how the dead configuration reached a release.

## Consequences

**A Go upgrade republishes every artifact.** That is the correct behaviour and it is not free:
storage grows by one copy of everything, and every consumer that pins a spec-hash tag sees it move
in the commit that moves the tag. Preferable to the alternative, which is every object wedged with
a conflict that mentions nothing about Go.

**CI catches this only because the digest is asserted.** If those assertions were ever relaxed to
"builds successfully", this class of change would ship silently and be discovered by an operator
upgrading a cluster.

**For the whole period `pushedWithin` was configured, it did nothing.** Anything reasoned about
retention in that window — including the claim in ADR 0054 that push recency covers content between
builds — was reasoning about a rule that was not running. What actually protected freshly published
content was the refresher.

**Reading a dependency's source beats reading its documentation**, when a misconfiguration fails
silently. Both halves of this record are configurations that looked right. Two of three zot
assumptions checked that afternoon turned out to be wrong, and neither was visible in a passing
test.

## Alternatives rejected

**Pinning the Go version indefinitely.** Trades a manageable, scheduled republish for an
accumulating security-patch debt, and the wedge arrives anyway the first time the pin is broken.

**Defaulting `onConflict` to `Overwrite` so the collision resolves itself.** It would make the
symptom disappear by silently replacing content under an unchanged name, which is the failure
[ADR 0054](0054-name-it-after-you-push-it.md) exists to prevent.

**Keeping two `keepTags` entries and giving them disjoint patterns.** Works, and is harder to read
than one entry with two rules while buying nothing.
