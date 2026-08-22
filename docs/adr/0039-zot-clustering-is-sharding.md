# 39. zot clustering is sharding, not replication

Date: 2026-08-21

## Status

Accepted. Revisits the "deliberately plain: no HA, no object storage, no TLS termination" position
the chart has held since [0030](0030-a-real-registry-serves-both-kinds.md).

## Context

The bundled registry has been a single pod with a single ReadWriteOnce volume, and the chart said
so: scaling it out meant running a different registry. The request was to make HA configurable in
values.

zot does support scaling out, from v2.1.0, and it is worth being precise about what that is,
because the name people reach for is wrong.

**It shards by repository name.** Each member hashes a repository path with SipHash and consults a
table to find which member owns it; a member receiving a request for someone else's repository
proxies it. So N members means each repository lives on exactly **one** of them. A member that is
down makes roughly 1/N of the repositories unavailable, and zot's own documentation states the
cluster is **not self-healing** — a failed member wants a restart or a resize.

It also has prerequisites this chart does not install:

- **S3-compatible object storage.** BoltDB, the local default, cannot be shared between processes.
- **A shared cache driver** — DynamoDB, or **Redis**, which is the one that matters here because it
  is the option that does not require AWS.
- Identical configuration on every member, stable per-member addresses, and a CA members verify
  each other with.

## Decision

**Support it, call it `registry.cluster`, and describe it as sharding everywhere it appears.**

Naming it `ha.enabled` would promise the one property it does not deliver, to exactly the readers
least likely to read the paragraph underneath. The values file, NOTES and `docs/registry.md` all say
"sharding, not replication" and name the 1/N consequence.

**The registry becomes a StatefulSet unconditionally**, at one replica when clustering is off.

Making the kind conditional would put a landmine under `cluster.enabled=true`: Helm would create a
StatefulSet while the old Deployment's ReplicaSet still owned pods matching the same selector, and
the two controllers would fight over one ReadWriteOnce volume — on the day the operator is already
changing storage, cache and TLS. Doing it once, in its own change, with a `lookup`-based guard that
names the `kubectl delete deployment` to run, is a migration an operator can follow.

**Never `volumeClaimTemplates`.** They would create `data-<sts>-0` and orphan the existing PVC —
which for `ImageBuild` is the system of record, since its output is an observation and cannot be
rebuilt from its spec ([0025](0025-dockerfile-builds-as-a-second-kind.md)). It would look exactly
like total data loss with the bytes still sitting in a volume nobody is looking at. The named PVC
stays a plain volume, and a test asserts there are no claim templates.

**Five refusals**, each for a combination that renders and then does not work:

| Refused | Because |
|---|---|
| clustering without S3 | BoltDB cannot be shared; two members on one volume disagree rather than fail |
| clustering without a shared cache | members disagree about which blobs exist, surfacing as intermittent 404s |
| clustering with the RWO PVC still enabled | it cannot be mounted twice — **and silently dropping it could discard an `ImageBuild`'s only copy**, so the operator has to say it |
| clustering without TLS | members proxy authenticated writes to each other, so the password crosses the network on every proxied write — threat I7 reopened inside the thing that closed it |
| a `hashKey` of any length but 16 | SipHash-2-4 takes a 128-bit key |

**Incidental win:** a StatefulSet's rolling update terminates pod-0 before creating its
replacement, so the ReadWriteOnce deadlock that forced `strategy: Recreate` stops existing.

## Consequences

**The highest risk is not in this chart, and the chart cannot check it.** `extensions.search`
records the pull timestamps the retention policy's `pulledWithin` depends on — the mechanism
[0031](0031-the-retention-guarantee.md) rests on. In clustered mode that metadata moves out of
per-pod BoltDB and into the shared cache driver. **If Redis is not persistent, a Redis restart loses
every pull timestamp**, every image then looks unpulled, and the next GC pass reclaims images live
objects still reference — on a `gcDelay` fuse rather than a retention window.

That is ADR 0031's failure mode, reintroduced by a component this chart does not manage, in the
deleting direction. It is stated in `values.yaml` next to `cache.redis`, in NOTES whenever Redis is
selected, and in `docs/registry.md`. It should be measured against a real clustered install before
anyone relies on it; concurrent GC by several members over one bucket is not something to assume.

**`cluster.tls.cacert` is not `http.tls.cacert`.** One word apart, opposite meanings: the second
makes zot demand a client certificate from every caller, including the kubelet, which has none —
every pull in the cluster stops. There is a test asserting the http block has no `cacert`.

**Members are addressed on the container port, not `registry.service.port`.** They talk to pods
directly through the headless Service and the Service port is not involved. The two coincide today
only because the container port is fixed.

**The headless Service sets `publishNotReadyAddresses`.** Members must resolve each other during
startup, before any of them is ready; without it a cold cluster never forms, because every member
waits for peers DNS refuses to name until they are ready and none of them ever is.

**S3 credentials are environment, never configuration.** The distribution driver honours the
standard AWS chain, so the config file omits `accesskey`/`secretkey` entirely — the same rule
`TestChartCredentialsAreNotFlags` already enforces for the composer's own S3, for the same reason: a
ConfigMap is visible in every `kubectl describe`.

**Honest framing, which belongs in the values file and does:** anyone who already runs S3 and Redis
could equally run a registry of their own and set `publish.mode: external`. This exists so the
bundled registry is not a *forced* single point of failure — not because this chart is the right way
to run a registry estate.

## Alternatives rejected

**Call it `ha`.** It is not.

**Switch kinds only when clustering is enabled.** Puts the migration under the value most likely to
be flipped by someone changing four other things at once.

**Silently disable `persistence` when clustering is on.** The quietest possible way to discard an
`ImageBuild`'s only copy.

**Ship Redis and MinIO as subchart dependencies.** They are real systems with real operational
requirements — persistence above all, per the risk above — and a chart that installed a
non-persistent Redis by default would ship the deletion bug as a default.
