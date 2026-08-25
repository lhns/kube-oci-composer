# 41. The registry has one writer and many readers

Date: 2026-08-24

## Status

Accepted. Supersedes [0039](0039-zot-clustering-is-sharding.md), whose reading of zot's cluster mode
is unchanged and is exactly why this record does not use it.

## Context

Nodes were cordoned on a real cluster and the bundled registry was rescheduled in the same batch as
the pods that needed to pull from it. Those pods sat in `ErrImagePull` waiting for the one thing
that could serve them. The requirement that came back was specific: **while the cluster is
rebalancing or cordoning a node, the whole registry must stay up — every image, not a fraction.**

That rules out 0039's answer. `registry.cluster` is zot's scale-out mode, which shards by
repository-name hash and proxies to the owning member, so a member going down takes ~1/N of the
repositories with it. 0039 named this honestly and shipped it anyway, on the grounds that the
bundled registry should not be a *forced* single point of failure. Honest labelling does not make it
the requested property.

The obvious replacement was zot's other documented multi-instance mode: N stateless replicas behind
one Service over shared storage, where every replica serves every repository. zot documents it, and
it is what "highly available registry" normally means.

**We tested it before building it, and it does not hold.**

## What the spike found

`test/spike` runs three zot instances over one shared volume with a shared Redis metadata store —
deliberately the most forgiving environment replication will ever see: one inode namespace, one page
cache, real POSIX locks. A failure there is conclusive for every real filesystem and for S3.

Concurrent pushes of distinct tags into one repository, routed to two instances, **lost 2–4% of tags
that had returned `201`**, on every clean run, with a further 1–2% rejected outright as
`MANIFEST_INVALID` or a bare `500`. The same concurrency against a *single* instance lost nothing.
Sequential pushes across all three lost nothing.

The mechanism is visible in zot's source. `ImageStore` serialises repository writes with an
in-process `*sync.RWMutex` and nothing else — there is no `flock` anywhere in
`pkg/storage/imagestore` — and `GetIndexContent` carries the comment *"the caller function MUST lock
from outside"*. A manifest push is a read-modify-write of `<repo>/index.json`, written whole-file to
a temporary path and renamed. Two processes read the same index, both splice in their own tag, the
second rename wins, and the first tag is gone while every instance agrees it was never written.

**Sharding is what made that lock sufficient**: exactly one process ever owned a repository.
Replication removes the invariant. This is not a filesystem problem and object storage does not fix
it — the race is between processes, not between clients of a disk.

A second finding matters as much: **garbage collection is a writer too.** It rewrites `index.json`
to drop untagged manifests, so an instance that only serves pulls is still mutating the store if it
collects. A probe that kept content alive by pulling it continuously — the exact bargain ADR 0031
rests on — still lost one image in 793 to collectors on other instances, *despite it being actively
refreshed*.

Blob handling came out clean: no layer was ever lost or corrupted. Local dedupe uses hardlinks and
`isBlobOlderThan` fails closed when `StatBlob` errors. The damage is confined to the index.

## Decision

**Exactly one registry instance may mutate the store. Any others serve reads only.**

- The writer takes every push and is the only instance with `gc: true` and a retention policy.
- Read replicas render `gc: false`, carry no retention policy, and receive no pushes.
- All of them share one store and one Redis metaDB (BoltDB is a file one process opens
  exclusively, so it cannot be shared, and per-replica metadata would break `pulledWithin`).

Reads and refreshes fan out across every instance. Under the same constraint the spike pushed 348
images through 4,881 refresh pulls with nothing lost and nothing corrupted, while the two-mutator
probe kept failing against the same running stack — which is what makes the pass attributable to the
constraint rather than to a quieter environment.

`registry.cluster` is removed rather than kept alongside. Two multi-instance modes, one of which
silently drops content, is a trap; and 0.5.0 is not yet tagged, so nothing released depends on it.

## Consequences

**What this buys.** A node drain no longer takes pulls down, and pulls are what fails visibly — the
`ErrImagePull` that started this. Any surviving replica serves every image.

**What it does not buy, stated plainly.**

- **Write availability is unchanged.** If the writer is moving, pushes fail. That is tolerable only
  because the reconcile is idempotent and retries: a failed push is a delayed publish, not a lost
  one. It is not tolerable for anything that needs synchronous publication.
- **Write throughput is that of one process.** The spike pushed 348 images through one mutator where
  three managed 793.
- **The shared store becomes the availability floor.** Replacing one registry pod with three pods
  over one volume moves the single point of failure; it does not remove it. This is only a win where
  the store is itself redundant.

**What the chart cannot check**: that a `ReadWriteMany` claim is genuinely honoured rather than bound
and backed per-node; that the filesystem's locking behaves; that Redis is persistent. Each is stated
where an operator will meet it, and refused where it can be detected at render time.

**A guard that was removed.** Clustering required `tls.enabled` because sharded members proxied
authenticated writes to each other across the pod network. Replicas never talk to each other, so at
N instances the exposure is identical to one and the rule has no remaining reason. Recorded here so
that its absence reads as a decision rather than an oversight.

## Alternatives rejected

**Symmetric replication** — the design this record was expected to adopt. Rejected by measurement,
not by argument.

**Keeping sharding alongside** — two modes, one of which does not deliver availability, doubles the
ways to choose wrong.

**A dedicated collector separate from the writer** — the writer already collects, and a second
mutating process is the thing the spike disqualifies.

**Shipping Redis as a subchart** — 0039's reasoning holds and gets stronger now that Redis is
mandatory rather than optional. A chart that installed a non-persistent Redis by default would make
the retention-deletion failure (threat D9) the default path to availability.

**Doing nothing beyond placement.** Scheduling controls and a PodDisruptionBudget stop the registry
being drained alongside its consumers, which is worth having on its own and shipped first. They
narrow the window; they do not remove it.
