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
| [0003](0003-one-ordered-layers-list.md) | One ordered `layers` list, no separate `base` field | Accepted |
| [0004](0004-two-kinds-two-controllers.md) | Two kinds, two controllers, two charts, one repo | Accepted |
| [0005](0005-go-controller-runtime.md) | Go and controller-runtime | Accepted |
| [0006](0006-push-is-optional.md) | `push` is optional; a built-in endpoint means no registry is required | Accepted |
| [0007](0007-packaging.md) | Packaging: OCI chart and image published to ghcr | Accepted |
| [0008](0008-supply-chain.md) | Supply chain: referrers for SBOM and signatures; key-based signing, not keyless | Proposed |
| [0009](0009-flux-conventions-without-dependency.md) | Flux conventions without a Flux dependency, and the name | Accepted |
| [0010](0010-workloads-reference-digests.md) | Workloads reference digests, never tags | Accepted |
| [0011](0011-content-tags-expire.md) | Content tags expire: mark-and-sweep garbage collection | Accepted, amends 0010 |
| [0012](0012-keep-pkg-registry.md) | Keep `pkg/registry`; own only the blob handler | Accepted |
| [0013](0013-persist-manifests.md) | Persist manifests so published builds survive a restart | Proposed |
| [0014](0014-pluggable-storage.md) | Pluggable storage and a two-tier input cache | Accepted |

## Format

    # NNNN. Title
    ## Status
    ## Context      — the forces, including measurements where they exist
    ## Decision     — what we do
    ## Consequences — what this costs, stated plainly
    ## Alternatives rejected — and why

Measurements beat assertions. Where a record says something was tried and did not work, it was
actually tried; see 0011 for an example of a feature that was built, measured to have no effect,
and removed.
