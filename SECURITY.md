# Security policy

## Reporting a vulnerability

Report privately via GitHub's "Report a vulnerability" on the Security tab, not as a public issue.

## Threat model

This controller publishes artifacts that other workloads run, so it sits in the supply chain.
What that means concretely:

**Inputs are content-addressed.** Every layer is verified against its digest before use, and a
mismatch is terminal — the object goes `Stalled` and nothing is published. A compromised upstream
serving different bytes is detected, not consumed.

**Only the operator can write to the registry.** Everything is published to a registry
([ADR 0035](docs/adr/0035-a-registry-is-the-only-publication-path.md)). The bundled zot allows
anonymous reads and authenticated writes, and only the controllers, plus the build Jobs they
create, get the write credential. Nothing else can push into it. Without TLS that credential
crosses the pod network in the clear; see [docs/registry.md](docs/registry.md).

**Your own registry means your own credential.** With the bundled registry disabled, the
controllers push to yours with the Secret you supply
([ADR 0034](docs/adr/0034-a-default-registry.md)). A compromise of the operator then means
poisoned images that other workloads run. Scope that token to the repositories it needs: push, no
delete.

**Secrets are read uncached, by name.** The cache is disabled for Secrets, so neither controller
needs `list` or `watch` on them. The composer only reads them. The builder also creates and updates
each build's own Secrets ([ADR 0050](docs/adr/0050-a-builds-secrets-belong-to-the-build.md)).

**Attestations are opt-in and enforce nothing alone.** SBOM, provenance and key-based cosign
signing exist and are off by default ([ADR 0008](docs/adr/0008-supply-chain.md),
[ADR 0040](docs/adr/0040-the-supply-chain-work-is-worth-building.md)). A signature protects nothing
until admission control verifies it, and the chart ships no admission policy.

**Release images are checked before they are tagged.** Each platform's binary in the published
image has its ELF architecture verified before any version tag or `latest` points at it.

## Supported versions

Pre-1.0. Fixes land on `main`; there are no backports.
