# 0021. Is active/standby enough for the serving endpoint?

## Status

**Accepted**, in favour of shared storage: with `--shared-storage` (implied by the S3 backend)
**every replica serves**, and only publishing, garbage collection and status writes stay on the
leader. Active/standby remains the default, because it is the only correct behaviour when the store
is node-local.

What forced the decision was spec-hash tags ([ADR 0017](0017-updating-the-consumed-digest.md)). The
reasoning below quietly assumes pulls are rare — an artifact is fetched once and cached by
containerd — which holds while the reference is stable. It stopped holding once *every* change to a
composition began producing a new tag: a new tag is a new pull, on every node running the workload,
every time anything changes. A single-point-of-failure registry is a different proposition under
that load than under the one assumed here.

Two things had to be true, and the second is easy to miss:

1. **Blobs reachable from every replica** — an RWX volume or S3. Asserted rather than detected,
   because a process handed a directory cannot tell whether it is node-local or a shared mount.
2. **Manifests present on every replica.** They live in the upstream registry package's in-memory
   map, *not* in the store, so shared blobs alone would leave a standby answering 404 for content
   sitting right there. `controller.StandbyReplay` refills that map from `status.history` — the
   same source the leader's own post-restart replay uses — and runs without leader election.

The chart now **fails** on `replicaCount > 1` without shared storage, rather than silently building
the trap described below.

## Context

The serving endpoint runs under leader election, so only one replica listens. A standby neither
reconciles nor serves, and stays out of the Service because readiness gates on having built the
artifacts ([ADR 0006](0006-push-is-optional.md)). Failover works without any additional mechanism,
which is a pleasing outcome — but it means:

- **Throughput does not scale.** Every pull in the cluster is served by one pod.
- **Failover is not instant.** A new leader must rebuild and republish before it reports ready. The
  layer cache makes that fast, and [ADR 0013](0013-persist-manifests.md) means older builds are
  replayed rather than lost — but it is seconds to minutes, not zero.
- **`replicaCount` is a trap.** The chart defaults to 1 and documents active/standby, but nothing
  stops someone raising it and expecting more capacity.

Two things make this less alarming than it sounds. Spegel mirrors anything already pulled
peer-to-peer, so the endpoint is not in the path for most pulls after the first. And artifacts are
pulled at pod start, not continuously — the load is bursty and small.

The constraint is not really leader election; it is that manifests live in the embedded registry's
in-memory map ([ADR 0012](0012-keep-pkg-registry.md)). Two replicas cannot share them even with
shared blob storage, which is why S3 for blobs does not unlock scale-out on its own.

## Options

**A. Leave it.** Document active/standby clearly, and treat throughput as a non-problem because
Spegel absorbs it.

- Costs nothing. Relies on an assumption about load that has never been measured.

**B. Make every replica serve, sharing storage.** Requires manifests in the shared store rather
than in memory — which means either writing our own read-only distribution handler, reversing
0012, or seeding every replica's in-memory map from the manifest store at startup and on change.
The latter is close to what replay already does and might be most of the way there.

- Real scale-out and instant failover.
- Reopens the decision 0012 made, and 0012's measurement — that the map is repopulated cheaply by
  pushing to it — is evidence *for* the seeding approach rather than against.

**C. Do not serve at all; require `spec.push`.** Let a real registry handle availability. The
built-in endpoint stops being a supported production path and becomes a convenience for small
clusters.

- Contradicts ADR 0006, which is one of the project's better arguments.

## What would decide it

- **Measured pull load on a real cluster.** If Spegel absorbs it as expected, A is correct and the
  rest is speculation.
- **Whether failover latency ever matters.** It only bites when the leader dies *and* a pod is
  starting *and* the content is not already on that node. That conjunction may simply never occur.
- **Whether B is as small as it looks.** Seeding replicas from the manifest store reuses the replay
  machinery; if a prototype confirms that, B becomes cheap enough that A stops being obviously
  right.

## Consequences of leaving it open

`replicaCount` stays a value that looks like scale-out and is not. The chart says so in a comment,
which is weaker than the API refusing it. If A is confirmed, the chart should probably reject
`replicaCount > 1` outright unless someone opts in.
