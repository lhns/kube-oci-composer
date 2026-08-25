# 42. Which sources we resolve, and which source-controller resolves

Date: 2026-08-25

## Status

Accepted. Narrows the Flux-only rule on `ImageBuild.spec.context` introduced with
[0025](0025-dockerfile-builds-as-a-second-kind.md); the OCI-artifact consequence is
[0043](0043-an-oci-artifact-is-a-source-we-own.md).

## Context

The two kinds have accumulated source verbs, and Flux's source-controller has its own set. Nothing
wrote down where the line is, so the vocabularies drifted and the drift looked like an oversight:

| Content | Ours | Flux |
|---|---|---|
| HTTP blob at a digest | `fetch` | — |
| ConfigMap | `configMap` | — |
| Container image at a digest | `image` | — |
| Git | — | `GitRepository` |
| OCI artifact | — | `OCIRepository` |
| Bucket / S3 | — | `Bucket` |

The question that forced this record was whether to add a `git` verb, and behind it a sharper one:
`ImageBuild.spec.context` accepts *only* a Flux source, and the doc comment justifying that says

> From a Flux source, so the revision is content-addressed and its digest is what makes the input
> hash meaningful. There is no inline or URL form: an unaddressed context would leave nothing to
> hash, and every reconcile would be a build.

That argument is about **addressability**, and it is right. But it was applied as "must be a Flux
source", which is strictly narrower than the reason supports — every `ImageComposition` layer source
is already content-addressed. `FetchSource.Digest` is required, `image` requires a digest by CEL, a
ConfigMap's content is hashed by the controller. A `fetch` with a declared digest satisfies the
stated bar exactly as well as a Flux artifact does. The rule was written for the one source that
existed when it was written.

## Decision

Two rules decide every source, and they are the record.

> **1. If the spec names the exact content, we resolve it. If a mutable ref has to be watched and
> resolved over time, source-controller does.**
>
> **2. We only implement a source whose address can be verified against the bytes we got.**

Rule 1 puts polling, credential-holding, revision tracking and status-reporting on the component
built for it. Rule 2 is what keeps `output = f(spec)` honest: `fetch` verifies a sha256, `image`
verifies a manifest digest, `configMap` hashes exactly what it read.

Applied to `ImageBuild.spec.context`, the rule becomes **"every build input is content-addressed"**
rather than "every build input is a Flux source" — the same principle, applied to one field.

### What the rules decide

**Git → Flux.** It fails both. A branch or tag needs tracking; and even a commit-pinned clone fails
rule 2, because a commit identifies a *tree* while materialising it runs through `.gitattributes`
filters, `core.autocrlf`, submodule choices and mode handling — none checkable against the commit
without implementing git's object model. `GitRepository` publishes a content-addressed tarball,
which is exactly the bytes-plus-digest shape rule 2 wants: source-controller has already done the
work of making git reproducible, and a raw clone hands that problem back.

The objection *"we already have our own `fetch`, so why not git"* is fair, and is where the line
falls. `fetch` is an HTTP GET, a sha256 comparison and an untar — deterministic by construction,
because the bytes either match the digest or they do not. Git is protocol-v2 negotiation, pack and
delta resolution, shallow clones, submodules, LFS and SSH, plus a working-tree materialisation that
must be *engineered* to be reproducible and cannot be verified afterwards. `go-git` is roughly
100k lines and still has protocol gaps.

**Buckets → Flux.** A prefix is a mutable ref, and a listing needs polling and credentials. Rule 1.

**OCI artifacts → ours.** An artifact pinned by digest passes both rules, and there is no tracking to
delegate. It is not implemented yet; see 0043.

**HTTP blobs, ConfigMaps, container images → ours**, as today.

## Consequences

**`ImageBuild.spec.context` becomes a union** whose members are content-addressed, rather than a
bare Flux reference. It also becomes **optional**: a Dockerfile that only declares a pinned `FROM`
and runs commands reads no files, and an empty context is addressed by construction, so the original
objection never applied to it. Requiring one meant pointing a Flux source at an empty directory.

**Not running Flux stops meaning "no git".** source-controller installs on its own —
`flux install --components=source-controller`, or its plain manifests — without the rest of Flux.
That is the answer to the need behind the `git` request, and it is a documentation change rather
than a feature. Said in the README and the chart NOTES.

**A source we decline is now a decision with a reason**, not a gap. The next person who notices a
Flux kind we lack has this table to read instead of re-deriving the argument.

**The rules can be wrong.** What would reopen git specifically: a concrete case source-controller
cannot serve — a protocol or auth shape it lacks, or an environment where running one more
controller is genuinely impossible. The design in that event is a required `commit`, `go-git` with a
shallow fetch, one shared package for both controllers, a dependency guard in the shape of
`internal/attest/deps_test.go`, and the determinism list above pinned down in code rather than
discovered. That is its own record and its own plan.

## Alternatives rejected

**A `git` verb now.** Rejected above. The door is documented rather than shut.

**Overloading `fetch` with a `git+https://` scheme.** It would make `fetch.digest` mean two
different things depending on the URL — a sha256 over bytes in one case, nothing checkable in the
other. `git archive` output is not stable across git versions, so there is no canonical byte stream
to digest.

**Leaving the Flux-only rule in place because it is stricter and therefore safer.** Strictness that
does not follow from the stated reason is not safety, it is an unexplained restriction — and this
one made a legitimate spec impossible while the reason it cited was satisfied.
