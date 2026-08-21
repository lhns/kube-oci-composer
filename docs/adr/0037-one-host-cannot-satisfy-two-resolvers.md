# 37. One host cannot satisfy two resolvers

Date: 2026-08-21

## Status

Accepted. Amends [0034](0034-a-default-registry.md), and corrects the host story in
[0030](0030-a-real-registry-serves-both-kinds.md) and
[0033](0033-one-chart-one-namespace.md).

## Context

`status.artifact.ref` is a single string, and two different resolvers have to make sense of it:

| Who | Resolves with | Needs it for |
|---|---|---|
| The controllers | cluster DNS | pushing, the tag-conflict check, and the retention refresh |
| The kubelet | the **node's** resolver | pulling, for every workload |

The chart fed both from one value, `registry.host`. That meant there was **no correct setting**:

- Leave it unset and references name the in-cluster Service. The controllers work; no Pod can pull.
  The publish succeeds and the failure surfaces later, elsewhere, as `ErrImagePull`.
- Set it to a name the nodes resolve and the controllers cannot resolve it. Every object fails with
  `no such host` from the cluster's DNS server *before publishing anything*.

Both halves were hit in order, on consecutive e2e runs, and the second was papered over with a
CoreDNS `hosts` entry teaching cluster DNS the node-facing name.

**A second failure hid behind the first, and it was the more dangerous one.** `insecureRegistries`
only added the Service name to `--insecure-registry` when *neither* host value was set. So setting
`registry.host` to a plain-HTTP NodePort produced a controller that could not resolve the name and,
had it resolved, would have failed the TLS handshake. The helper's comment was right — *"registry.host
is deliberately NOT added: it may well be an ingress terminating TLS"* — it was simply reasoning
about the public name while being applied to a value that was also the internal one.

## Decision

**Split the two addresses. They were never the same thing.**

- `--default-registry` is where the **controllers** connect. With the bundled registry it is always
  the in-cluster Service, and nothing an operator sets changes that.
- `--public-registry-host` is what a **workload** is told to pull. It reaches
  `status.artifact.ref` and `status.artifact.tags` and nothing else — the controllers never open a
  connection to it and may well be unable to resolve it.

The mechanism was already in the code and unused: `target` has carried `writeRepo` and `pullRepo`
since the serving endpoint existed, read in exactly the right places — `writeRepo` by the push, the
credential rule and the conflict check, `pullRepo` only by `artifactStatus`. Re-populating
`pullRepo` was a few lines. `ImageBuild` had no such split and gains one.

**Only the operator's own registry is rewritten.** `DefaultRegistry.PublicRepository` compares on
host, exactly as the credential rule does, and returns anything else unchanged. An object that
named a repository elsewhere is reported back as written; the operator's public name says nothing
about a registry the operator does not run.

**And the credential rule keeps comparing against the INTERNAL host.** Treating the public name as
authority too would mean a tenant who wrote the operator's public name into their own spec could be
handed the operator's credential for a connection the operator never verified. The public name is
documentation, not authority. There is a test.

**`registry.publish.mode` has no default, and the chart refuses to install until it is set** —
`ingress`, `nodePort`, `external` or `internalOnly`, each asserting the values that make it true.

That is a deliberate breaking change: `helm install` with no arguments stops working. It is the
point. The chart currently "works" with no arguments and produces images nothing can pull, which is
the failure this record is about. Which mode is possible depends on the cluster — is there an
ingress controller, does DNS serve a name for it, will someone put a file on every node — and no
default is right everywhere. So the failure moves from a silent runtime one to a message at install
time naming the four choices.

## Consequences

**The e2e's CoreDNS workaround is deleted, and its absence is the test.** The controllers no longer
resolve the public name, so there is nothing to work around. A suite that passes without those
twenty lines is evidence the split is right; one that does not would be evidence it is not.

**`insecureRegistries` simplifies to something true.** The Service name is always what the
controllers talk to, so it is always on the list — until TLS exists to take it off. The conditional
that produced the second failure is gone.

**Upgrading an existing install requires one new value.** `helm upgrade` fails until
`registry.publish.mode` is set, with a message naming the options. Anyone already setting
`registry.host` to a node-resolvable name keeps it — it now means only what they thought it meant —
and adds `publish.mode: nodePort`.

**An ingress is a first-class mode again.** [0006](0006-push-is-optional.md) claimed, as the best
property of the serving endpoint, that it needed no node configuration at all. That was never about
the endpoint: it was about being behind an ingress with a certificate the nodes already trust. The
`ingress` mode is that property, kept, and it is the only mode needing nothing on the nodes.

**The name in every example is `oci-composer.internal`, not `oci.internal`.** A cluster may run
other registries, and the example should not read as though this were the only one.

## Alternatives rejected

**Keep one host and document the requirement that it resolve in both places.** This is what the
previous version did, and it is what the docs said. It puts a cluster-DNS entry for a node-facing
name on the operator's checklist forever, and the failure when they miss it is `no such host` on
every object — which reads as the controller being broken.

**Default `publish.mode` to `internalOnly`.** Installs cleanly, and produces images nothing can
pull, silently, which is the exact failure being removed. A default that is wrong for almost
everyone is worse than no default.

**Derive the public host automatically** — from a NodePort and a node IP, say. Node IPs change,
there is more than one, and nothing about the chart's inputs says which is reachable from where.
A guess here fails in a way that looks like a registry fault.
