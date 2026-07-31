# 0014. Pluggable storage and a two-tier input cache

## Status

Accepted.

## Context

Two things need to keep bytes: the served blob store, and the fetched layer sources.

Neither is a system of record — composition is deterministic, so anything lost can be rebuilt. But
"can be rebuilt" turned out to be doing a lot of work in the original design, because rebuilding
means re-fetching every layer from upstream. Two measurements made that concrete:

- Before the input-hash short-circuit (0002), **every** reconcile re-downloaded every layer.
  Nothing had changed and it still pulled tens of megabytes hourly, because the output digest could
  only be learned by assembling.
- After a restart the store is empty, so the startup reconcile re-fetches everything before the
  endpoint can serve anything — and readiness correctly holds the pod out of the Service while it
  does (0006).

Both are network cost for content that is content-addressed and therefore trivially cacheable.

## Decision

**One `Store` interface — `Stat`, `Open`, `Write`, `Delete`, `List` — with disk, in-memory and S3
backends, and two key namespaces (`inputs/`, `blobs/`) so one backend serves both roles.**

A shared conformance suite runs against every backend, because the point of the interface is that
callers cannot tell them apart. A backend that only passes its own tests is not substitutable.

Contract details that exist because a caller depends on them:

- **A miss is `ErrNotFound`, not a backend-specific error.** Callers treat a miss as ordinary
  control flow: the cache falls through to the origin, the endpoint returns 404. Anything else
  turns both into failures. (This is why the S3 backend `Stat`s inside `Open` — minio-go's
  `GetObject` is lazy and would otherwise report a miss only on first read.)
- **Delete is idempotent.** A sweep is not a transaction, and a key vanishing between the listing
  and the delete must not fail the cycle.
- **List is complete or it fails.** A partial listing reads as "these objects do not exist", which
  would make the collector delete live content.
- **Writes are atomic**, so a reader never sees a partial object. In-flight temp files are excluded
  from listings for the same reason.

**S3 uses minio-go**, not the AWS SDK: the target is usually Ceph RGW or MinIO rather than AWS,
path-style is its default for custom endpoints, and the dependency tree is a fraction of the size.
Configuration is validated at startup — a missing scheme is rejected rather than guessed, because
guessing wrong means silently shipping credentials in plaintext. Credentials come from the
environment, never from flags, which appear in `ps`, in the pod spec and in every
`kubectl describe`.

**The input cache is two-tier.** Assembly needs local file paths; durability wants object storage.
A local directory is always present, with an optional `Store` behind it. Lookup is ordered by cost:
local, remote, origin.

- A **local hit is trusted** without re-hashing. Content is verified on the way in, and
  re-verifying per reconcile reintroduces a smaller version of the cost being removed.
- A **remote hit is verified**, because that tier is shared and durable and may hold bytes this
  process never checked. A mismatch means corruption, so the entry is dropped rather than served,
  and the good copy replaces it on the way back from the origin.
- **Cache failures never fail a build.** Only the local tier changes what is returned; a broken
  object store costs durability across a restart and nothing else. Turning an optimisation into a
  dependency would be worse than the problem it solves.

## Consequences

A new dependency tree in a repository that previously had only `go-containerregistry` and the
Kubernetes libraries. Contained behind the interface, so the disk backend keeps working if it ever
needs replacing.

S3 for *served blobs* is supported but buys less than it appears to: manifests are in memory and
rebuilt at startup either way (0012, 0013), so it saves re-uploading layers rather than removing
the rebuild. The flag help says exactly that rather than implying more.

## Alternatives rejected

**A PVC and nothing else.** Works, and does not survive the controller moving to another node with
`ReadWriteOnce`, nor let anything be shared.

**Cache the assembled output instead of the inputs.** The output is cheap to recompute once the
inputs are local; the inputs are what cost network. Caching the output would also have to be
invalidated on every spec change, whereas inputs are immutable by digest.

**Re-verify local cache entries on every read.** Covered in 0002: cost without additional
guarantee.

**aws-sdk-go-v2.** More configuration surface and a credential chain we do not need, at several
times the dependency weight, for an endpoint that is usually not AWS.
