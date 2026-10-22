# The replication spike

Three zot instances over one shared store, to answer a question the documentation could not:
**is it safe to run more than one zot process against the same storage?**

It is not, if more than one of them writes. This directory is the evidence.

## Running it

```sh
cd test/spike
docker compose up -d
go test -tags spike ./test/spike/ -v -count=1 -timeout 20m
```

The build tag keeps it out of `make test` and `make e2e-test`: the soaks take minutes and would not
fit the e2e timeout.

## What it is, and what it deliberately is not

A shared Docker volume gives all three containers one inode namespace, one page cache and real POSIX
locks — **the most forgiving conditions replication will ever see**. That is the design: a failure
here is conclusive and applies to every real filesystem and to S3 as well, while a pass here proves
only that the zot-level question is settled.

It says nothing about cross-client coherence or distributed locking on a real ReadWriteMany
filesystem. Those are properties of the filesystem and need a real multi-node cluster.

## Why replication was in doubt

zot's `ImageStore` serialises repository writes with an in-process `*sync.RWMutex` and nothing else —
there is no `flock` anywhere in `pkg/storage/imagestore` — and `GetIndexContent` carries the comment
*"the caller function MUST lock from outside"*. A manifest push is a read-modify-write of
`<repo>/index.json`, written whole-file to a temp path and renamed.

Sharding is what made that lock sufficient: zot's scale-out mode hashes each repository onto exactly
one member, so exactly one process ever owned a repo. Replication removes that invariant.

## Findings

| Probe | Result |
|---|---|
| Concurrent writes, **two instances**, one repository | **~2–4% of tags silently lost**, plus 1–2% rejected with `MANIFEST_INVALID` or `500`, on every clean run |
| Same concurrency, **one instance** | nothing lost |
| Sequential writes across three instances | nothing lost |
| Read-after-write across instances, 200 iterations | 0 stale, by tag or by digest |
| Refreshed content vs. **collectors on every instance** | 1 image in 793 reclaimed **despite being actively pulled**; 0 blobs lost or corrupt |
| Refreshed content vs. **one collector**, writes to one instance | 0 lost in 348, across 4,841 refresh pulls |
| Pulls while a **read replica** is stopped | 40/40 served, 0 missing |
| Pulls while the **writer** is stopped | 40/40 served, 0 missing |
| Control: pulls while the **only** instance is stopped | 0 served, 10 unreachable |

The last three are the feature working: any surviving instance serves every image, including when
the single writer is the one that went away. The control is what makes them mean anything — a
survival test that passes with one instance is measuring nothing.

**Note on reproducing the multi-collector result.** `compose.yaml` now ships the *fixed* layout
(5001 writes and collects, 5002/5003 serve only), so re-running today gives a pass. To see the
failure again, set `"gc": true` in `conf-reader/config.json` and re-run
`TestARefreshedImageSurvivesEveryInstancesCollector`: three collectors over one store reclaim
content that is being actively pulled.

Two things follow.

**A tag can vanish after returning `201`.** Both writers read the same `index.json`, both splice in
their own tag, the second rename wins, and the first tag is gone. Every instance then agrees it was
never written. For `ImageComposition` that means a rebuild; for `ImageBuild`, whose output is an
observation that may not reproduce ([ADR 0025](../../docs/adr/0025-dockerfile-builds-as-a-second-kind.md)),
it means losing the only copy.

**Garbage collection is a writer too.** It rewrites `index.json` to drop untagged manifests, so an
instance that collects is mutating the store even if no push is ever routed to it. That is why the
refresh probe lost content even though each instance wrote its own repository — the loss was between
a writer and two remote collectors.

Blob GC itself came out clean: 0 layers missing or corrupt across every run. Local dedupe uses
hardlinks (`storeDriver.Link`), and `isBlobOlderThan` fails closed when `StatBlob` errors. The damage
is confined to the index.

## The shape that works

**Exactly one instance may mutate the store.** The others set `gc: false`, carry no retention policy,
and receive no pushes. Reads and refreshes still fan out across all of them, which is the part worth
having: a pull is what fails visibly when a node is drained.

`conf-writer/` and `conf-reader/` are the two configurations, and `compose.yaml` wires 5001 as the
writer with 5002/5003 as read replicas.

The cost is honest and worth stating: writes are serialised through one process, so push throughput
is that of a single instance — 348 images through one mutator against 793 through three. Availability
of *writes* is not improved either; if the writer is moving, pushes fail and are retried on the next
reconcile, which is safe because the reconcile is idempotent.

## Reproducing the failure

`TestOnlyOneMutatorMakesTheRestSafe` passes only because of the constraint, not because of anything
else about the environment. The proof is `TestTwoMutatorsLoseContent`, which runs against the same
stack and **asserts that loss happens** — it routes writes to two instances and expects tags to go
missing.

That inversion is deliberate. The measurement is the entire justification for ADR 0041, so what is
worth guarding is that the justification still holds. If that test ever fails, either the harness has
stopped exercising the race — in which case every other result here is void — or zot has learned to
coordinate writes across processes and the design should be revisited.

## Clean up

```sh
docker compose down -v
```
