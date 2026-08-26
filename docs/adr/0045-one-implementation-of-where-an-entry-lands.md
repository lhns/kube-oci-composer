# 45. One implementation of where an entry lands

## Status

Accepted. Restores the rule [ADR 0023](0023-more-archive-formats.md) already required.

## Context

Every `ImageBuild` with a `sourceRef` context was broken, in two ways that looked unrelated:

```
# no subpath   -> BuildKit:     failed to compute cache key: "/package.json": not found
# subpath: ui  -> the fetcher:  subpath "ui" is not present in the archive
```

The fetcher removed one leading path component from every entry whenever the context came from a
Flux source, on the belief that source-controller wraps its tree in a directory whose name cannot be
predicted. **It does not.** A `GitRepository` artifact carries a bare `.` and then files at the root.

So `package.json` — one component, no `/` — was taken for the wrapping directory itself and dropped.
Every root-level file disappeared. `ui/Button.tsx` became `Button.tsx`, so `subpath: ui` matched
nothing in an archive containing 266 entries under `ui/`. The fetch reported success, because nested
directories survived one level up, which is why it presented as a BuildKit cache-key error.

### The belief was never true, and the evidence was next door

`internal/oci` — the composer's assembler — has never stripped anything, and it consumes the same
Flux artifacts in production. One controller had the rule and the other did not, and only the one
with it was wrong.

### ADR 0023 predicted this precisely

> `subpath`, rebasing, mode normalisation, symlink handling, deterministic ordering and traversal
> refusal are the same code the tar path uses — extracted into `extract.go` so there is exactly one
> implementation of *where an entry is allowed to land*. **Two copies of that check would mean the
> next hardening reaching one of them.**

`internal/archive` was written as the second copy, for the builder's own fetcher. The next change to
path handling reached one of them, and was wrong.

### Four things agreed with each other, and none with reality

The package comment promised detection — *"It strips a segment only when there really is one such
directory"* — that the code never performed. It also claimed to be *"deliberately the same rule as
build.MatchesContextPath"*, which tried the exact match FIRST and so worked on both shapes; the
comment asserting they agreed is what let them diverge. The unit fixture built a wrapped archive.
So did the e2e fixture, which archived a directory rather than its contents.

Worst of all, the guard was there and was vacuous: `TestStripWrapperOnlyStripsARealWrapper` had a
subtest named *"files at the root survive"*, documented as *"must still arrive intact rather than
being silently emptied"* — calling `Extract` with stripping **off**. It exercised the branch that
could not fail.

## Decision

**One implementation, shared, and the spec decides how deep to strip.**

`archive.Mapping` is the single rule for where an entry lands: remove N leading path components,
then select `subpath`. The composer's `collector` and the builder's `Extract` both call it, and both
test suites drive their real extractors from one exported table (`internal/archive/archivetest`).

**`stripComponents` on `FetchSource`**, default 0, shared by `ImageComposition` layers and
`ImageBuild.spec.context.fetch`. Nothing infers depth from the source kind ever again. It is a
count of PATH components, not a tar flag: it lives in the shared mapping, so zip, deb, image layers
and every tar variant behave identically.

**Strip first, then subpath**, so `subpath` names a path as the build sees it rather than as the
archive stored it.

**A strip that removes every entry is refused**, alongside the existing refusal of a subpath that
matched nothing. A silently empty tree is what this whole bug was made of.

**`matchesContextPath` loses its tolerance.** Trying the exact match first and then falling back
looked helpful; it was how the Dockerfile kept being found while the context around it was emptied,
which is why the failure surfaced somewhere else entirely.

## Consequences

**`ImageComposition` gains a capability and a one-time rebuild.** Reaching into a release tarball
previously required `subpath: app-1.2.3`, carrying the version, edited on every bump;
`stripComponents: 1` says what is meant. The field is hashed unconditionally — no "only when set"
special case — so every composition rebuilds once on upgrade. Harmless, since `output = f(spec)`
makes the digest and tag identical and the push a no-op, but it is real work.

**The e2e fixture now models a real artifact**, archived from inside the directory rather than the
directory itself. That is what makes the suite able to catch this class; previously it asserted the
same wrong belief the code held.

**`internal/archive` still exists separately from `internal/oci`**, for the reason ADR 0025 gives:
the sinks differ, and the build path must not import the composer. What is shared is the path rule
only. Safety policy stays per-sink, deliberately: the composer rebases an absolute entry where the
builder refuses it, and keeps a symlink verbatim as inert layer data where the builder refuses one
that escapes the tree. Those depend on whether anything is written to a filesystem. Where an entry
lands does not.

**What would change this decision**: a Flux source kind that genuinely wraps its tree. Checked for
`GitRepository`; not verified against `Bucket` or `OCIRepository` on a live cluster. If one wraps,
the failure is loud and the answer is `stripComponents: 1` — the same contract every other archive
has.
