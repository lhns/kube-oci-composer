# 0005. Go and controller-runtime

## Status

Accepted.

## Context

The controller has to speak the OCI distribution protocol, assemble tarballs and image manifests,
serve a `/v2/` endpoint, and behave like a Kubernetes controller. The language choice is mostly
determined by which of those has the least substitutable library.

## Decision

Go, with `sigs.k8s.io/controller-runtime` and kubebuilder layout, matching the sibling operators
in this family (`kube-vnet`, `kube-ha-surge`).

The decisive dependency is **`github.com/google/go-containerregistry`**. It provides manifest and
layer construction, registry authentication, `remote.Write`/`remote.Head`, and a standards-
compliant registry implementation including the Referrers API. There is no comparable library in
another language, and reimplementing the distribution spec is exactly the kind of work that looks
small and is not.

controller-runtime brings the manager, caching client, leader election, health probes and the
`Runnable` lifecycle. The serving endpoint and the garbage collector are both registered as
`Runnable`s so the manager owns their lifecycle — a listener failure takes the process down rather
than leaving a controller that reports Ready for artifacts nothing can pull.

Layout follows the siblings so that anyone who has read one has read all of them: `api/`, `cmd/`,
`internal/`, `config/`, `charts/`, `docs/adr/`, `test/e2e/`; stdlib `flag` rather than cobra;
version stamped via `-ldflags`; unit tests as `X_test.go` and envtest as `X_integration_test.go`
behind a build tag.

## Consequences

Committed to the Go ecosystem and to controller-runtime's release cadence, which is the normal
cost of writing a Kubernetes controller.

## Alternatives rejected

**Rust with `oci-distribution`.** Better assembly ergonomics in places, but the Kubernetes
controller story (kube-rs) is less mature, and it would be the only non-Go operator in this family
— every future maintainer pays for that.

**A shell script wrapping `crane` or `skopeo`.** Viable for a one-shot job and not for a
controller: no conditions, no observed generation, no watch, no status. One of the sibling
projects is a shell script, and it is a shell script precisely because it has no API to reconcile.

**Kubebuilder scaffolding as-is.** Used as a layout reference, not generated wholesale. The
generated `main.go` carries webhook and cert-manager plumbing this controller does not need, and
deleting scaffolding is more error-prone than writing the thirty lines that are actually used.
