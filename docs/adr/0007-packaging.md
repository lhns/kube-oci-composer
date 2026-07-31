# 0007. Packaging: OCI chart and image published to ghcr

## Status

Accepted.

## Context

The controller has to be installable by the same GitOps machinery it serves, and consistently with
its sibling operators.

## Decision

- **Container image**: `ghcr.io/lhns/kube-oci-composer`, multi-stage build, `CGO_ENABLED=0`,
  distroless nonroot base, read-only root filesystem, all capabilities dropped.
- **Helm chart**: published as an OCI artifact to `ghcr.io/lhns/charts/kube-oci-composer`, so Flux
  consumes it with `OCIRepository` and no chart repository is needed. Fitting, given what this
  project is.
- **JSON schemas** published under `schemas/` for `kubeconform`, so a repository can validate
  `ImageComposition` manifests in CI before anything reaches a cluster.
- **Version stamping** via `-ldflags` into `main.version/commit/date`, surfaced by `--version`.
- **Chart values nested under `operator:`**, matching `kube-vnet`, with each key carrying a comment
  explaining the default and its trade-off, and a reference to the ADR that decided it.

The chart is **tested**, not just rendered: the rendered manifests and the RBAC are asserted
against what the controller actually needs, so the values-to-flags mapping cannot drift silently.

`replicaCount` defaults to 1 and is documented as **active/standby, not scale-out**. The serving
endpoint runs under leader election and its blob store is node-local unless S3-backed, so a
standby neither reconciles nor listens. It stays out of the Service because it never reports
ready, which makes failover work without a second mechanism (0006).

## Consequences

Tied to ghcr availability for installation. Mitigated by the artifacts being ordinary OCI content
that can be mirrored.

Chart tests mean chart changes require a Go test run, which is a small tax for the drift they
prevent.

## Alternatives rejected

**A traditional Helm repository on GitHub Pages.** Another moving part, and it would be an odd
choice for a project whose thesis is that OCI is a good distribution mechanism.

**Kustomize only, no chart.** `config/` is maintained for `kubectl apply -k` and for CRD
installation, but a chart is what the sibling operators ship and what makes the flag surface
discoverable.

**Alpine or Debian base.** Nothing in the binary needs a shell or libc. Distroless nonroot removes
an entire category of CVE noise from a component that sits in the supply chain.
