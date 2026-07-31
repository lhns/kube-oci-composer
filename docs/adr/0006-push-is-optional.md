# 0006. `push` is optional; a built-in endpoint means no registry is required

## Status

Accepted.

## Context

The obvious design is: the controller builds an artifact and pushes it to a registry. That makes a
registry a hard prerequisite. For a small cluster it means installing and operating a registry,
giving the controller write credentials to it, and having somewhere durable to keep its state —
all so that two tarballs can be combined into one artifact.

It also creates a genuinely new risk. Pull credentials already exist wherever a private registry
is used. What a push design adds is the cluster gaining the ability to **write** to a registry, so
a compromise means poisoning artifacts other workloads consume rather than reading one
application's data.

## Decision

**`spec.push` is optional.**

- **Omitted** — the controller serves the artifact itself from a built-in read-only OCI
  distribution endpoint. Nothing else to install, and no registry credentials anywhere. Workloads
  pull from an ordinary Service behind the cluster's existing ingress and certificate, so
  containerd fetches over HTTPS with a valid certificate and **no node configuration at all**: no
  `hosts.toml`, no DaemonSet, no containerd socket.
- **Present** — push to an external registry, for sharing beyond the cluster or for
  registry-native attestations (0008).

**Determinism is what makes serving mode cheap.** The controller keeps no durable state it cannot
rebuild: the artifact is a pure function of the spec, so a lost blob store costs a rebuild rather
than data. A registry has to persist bytes; a deterministic composer does not.

**What "disposable" does not mean.** The endpoint does **not** build on demand — a pull for
something absent returns 404, exactly as a registry would. What refills the store is the reconcile
that runs for every object once the cache syncs at startup. Because that costs a re-fetch of every
layer without a warm cache (0014), readiness holds the pod out of the Service until it has
happened; serving 404s to a workload merely waiting on a restart would put it into
`ImagePullBackOff` for no reason.

**Spegel makes this robust rather than fragile.** Once an image has been pulled onto any node,
Spegel serves it peer-to-peer, so controller downtime does not block workloads that already ran.
The controller is the seed and the authority; Spegel is the availability layer. That is strictly
better than a registry, which would have to be up *and* have its state intact.

Note what Spegel is not: a push target. It indexes what is already in a node's containerd content
store. It complements the controller and cannot replace it.

## Consequences

Serving mode gives up the **Referrers API** in practice, so SBOMs and signatures have nowhere
standard to attach (0008). Anything wanting registry-native attestations should set `push`.

It also inherits the constraints of the embedded registry implementation — see 0012 and 0013.

The endpoint is one more thing listening on the network. It is read-only to the Service, serves
reproducible content, and writes arrive only over loopback from the controller itself.

## Alternatives rejected

**Require a registry.** Simpler code, worse deployment. It makes the smallest useful case — one
cluster, one artifact — need two components and a credential.

**Write into each node's containerd content store.** Needs a DaemonSet and the containerd socket,
which is a far larger privilege than an HTTP endpoint, and it is per-node rather than per-cluster.

**Push to Spegel.** Not possible. Spegel mirrors what nodes already have and has no push endpoint
and no storage of its own.
