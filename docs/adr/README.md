# Architecture Decision Records

Numbered, immutable once merged. A decision that changes gets a **new** record that supersedes
the old one rather than an edit, because the reasoning is the part that decays and a quietly
rewritten ADR destroys the evidence of why something was believed.

Each record states what was decided, what it cost, and what was rejected. The rejected
alternatives are not padding — they are the part that stops the same argument being had twice.

| # | Decision | Status |
|---|---|---|
| [0001](0001-compose-dont-build.md) | Compose, don't build | Accepted |
| [0002](0002-content-addressed-inputs.md) | Every input is content-addressed; the output digest is a pure function of the spec | Accepted |
| [0003](0003-one-ordered-layers-list.md) | One ordered `layers` list, no separate `base` field | Partly superseded by 0016 |
| [0004](0004-two-kinds-two-controllers.md) | Two kinds, two controllers, two charts, one repo | Superseded by 0016 |
| [0005](0005-go-controller-runtime.md) | Go and controller-runtime | Accepted |
| [0006](0006-push-is-optional.md) | `push` is optional; a built-in endpoint means no registry is required | Accepted |
| [0007](0007-packaging.md) | Packaging: OCI chart and image published to ghcr | Accepted |
| [0008](0008-supply-chain.md) | Supply chain: referrers for SBOM and signatures; key-based signing, not keyless | Proposed |
| [0009](0009-flux-conventions-without-dependency.md) | Flux conventions without a Flux dependency, and the name | Accepted |
| [0010](0010-workloads-reference-digests.md) | Workloads reference digests, never tags | Accepted |
| [0011](0011-content-tags-expire.md) | Content tags expire: mark-and-sweep garbage collection | Accepted, amends 0010 |
| [0012](0012-keep-pkg-registry.md) | Keep `pkg/registry`; own only the blob handler | Accepted |
| [0013](0013-persist-manifests.md) | Persist manifests so published builds survive a restart | Accepted |
| [0014](0014-pluggable-storage.md) | Pluggable storage and a two-tier input cache | Accepted |
| [0015](0015-base-images-are-platform-pinned.md) | Base images are platform-pinned; config inheritance is opt-in | Accepted |
| [0016](0016-the-scope-line-is-determinism.md) | The scope line is determinism, not Dockerfile parity | Accepted, supersedes 0004 and part of 0003 |

## Open questions

Recorded in the same format, because the shape fits — context, options, consequences — even
though nothing is decided. Each states **what would decide it**, which is the part that otherwise
gets lost: an open question with no exit criterion just becomes background noise.

`Open` is not `Proposed`. A Proposed record is a plan waiting to be built; an Open one is a
question waiting for evidence.

| # | Question | Status |
|---|---|---|
| [0017](0017-updating-the-consumed-digest.md) | How does a workload's digest reference get updated? | Open |
| [0018](0018-multi-architecture-output.md) | Multi-architecture output | Open |
| [0019](0019-pod-reference-protection-revisited.md) | Should garbage collection protect Pod-referenced digests? | Open, reopened |
| [0020](0020-is-the-supply-chain-work-worth-building.md) | Is the supply-chain work worth building? | Open |
| [0021](0021-active-standby-or-shared-storage.md) | Is active/standby enough for the serving endpoint? | Open |
| [0022](0022-distro-packages-as-layer-sources.md) | Distro packages are a layer source: `unpack: deb` | Accepted |
| [0023](0023-more-archive-formats.md) | More archive formats: `unpack: zip` and the compressed-tar family | Accepted |
| [0024](0024-images-as-layer-sources.md) | An image can be a layer source, flattened | Accepted |
| [0025](0025-dockerfile-builds-as-a-second-kind.md) | Dockerfile builds, as a second kind with a weaker promise | Accepted, for an alpha |
| [0026](0026-a-source-artifact-can-lag-its-own-spec.md) | A source's artifact can lag its own spec, and the tag moved first | Accepted, sharpens 0009 and 0017 |
| [0027](0027-what-rootless-buildkit-actually-needs.md) | What rootless BuildKit actually needs | Accepted |
| [0028](0028-the-kind-is-called-imagebuild.md) | The kind is called ImageBuild | Accepted, reverses 0025's naming |
| [0029](0029-three-valued-tag-conflict-policy.md) | A tag conflict has three answers, not two | Accepted, amends 0017 |
| [0030](0030-a-real-registry-serves-both-kinds.md) | A real registry serves both kinds, and zot is the one we ship | Accepted, amends 0006 |
| [0031](0031-the-retention-guarantee.md) | Live objects' images are never reclaimed, by anything | Accepted, sharpens 0011 |
| [0032](0032-the-embedded-registrys-future.md) | The embedded registry stays, and stops being the default | Accepted, revisits 0012 |

## Format

    # NNNN. Title
    ## Status          — Accepted | Proposed | Open | Superseded by NNNN
    ## Context      — the forces, including measurements where they exist
    ## Decision     — what we do
    ## Consequences — what this costs, stated plainly
    ## Alternatives rejected — and why

Open records carry a **What would decide it** section instead of a Decision. If a question cannot
be given one, it is usually not yet a question — it is a preference.

Measurements beat assertions. Where a record says something was tried and did not work, it was
actually tried; see 0011 for an example of a feature that was built, measured to have no effect,
and removed.
