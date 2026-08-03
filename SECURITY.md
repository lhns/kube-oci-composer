# Security policy

## Reporting a vulnerability

Report privately via GitHub's "Report a vulnerability" on the Security tab, not as a public issue.

## Threat model

This controller publishes artifacts that other workloads run, so it sits in the supply chain.
What that means concretely:

**Inputs are content-addressed.** Every layer is verified against its digest before use, and a
mismatch is terminal — the object goes `Stalled` and nothing is published. A compromised upstream
serving different bytes is detected, not consumed.

**The default mode has no registry credentials.** With `spec.push` unset the controller serves
artifacts itself and never authenticates anywhere.

**Push mode grants write access to a registry.** This is the one genuinely new capability. Pull
credentials already exist wherever a private registry is used; what push adds is the cluster
being able to *write*, so a compromise means poisoning artifacts other workloads consume rather
than reading one application's data. Scope the token to a single repository, push-only, no
delete, and prefer serving mode where nothing outside the cluster needs the artifact.

**Secrets are read uncached, by name.** The controller needs `get` on secrets, not `list` or
`watch`. Caching would require watching every Secret in the cluster to read one push credential.

**The serving endpoint is read-only to the network.** Writes arrive over loopback from the
controller itself. The chart does not expose the write path.

**The embedded registry is `go-containerregistry`'s `pkg/registry`**, which upstream describes as
aimed at tests. That is stated plainly in [ADR 0012](docs/adr/0012-keep-pkg-registry.md) rather
than buried: the exposure is bounded — read-only, reproducible content, loopback writes — but
anyone uncomfortable with it should set `spec.push` and use a real registry.

**Attestations are not implemented.** SBOM, provenance and signing are designed
([ADR 0008](docs/adr/0008-supply-chain.md)) and not built. Even once they are, signing enforces
nothing until admission control verifies it.

## Supported versions

Pre-1.0. Fixes land on `main`; there are no backports.
