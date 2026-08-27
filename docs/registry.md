# The registry

Both kinds publish to a registry. **The chart installs one by default** — zot, with credentials it
generates — so a default install is a complete local system with nothing external to configure.

This page is what the operator owns: the one setting that is not automatic, the configuration that
is easy to get silently wrong, and what happens if you bring your own registry instead.

## The one thing that is not automatic

**How workloads reach the registry.** The chart refuses to install until you say, and everything
else has a working default.

The reason it asks rather than guesses: two different resolvers have to make sense of a registry
address, and they answer differently.

| Who | Resolves with | Needs it for |
|---|---|---|
| The controllers | **cluster DNS** | pushing, and refreshing to keep images alive |
| The kubelet | the **node's** resolver | pulling, for every workload |

The controllers are handled automatically — they always use the registry's in-cluster Service, and
nothing you set here changes that. The kubelet is the open question, because containerd resolves
image references with the node's resolver, which does not see cluster DNS. A Pod cannot pull from a
`.svc.cluster.local` name however healthy the Service is.

Which of those is possible depends on your cluster, not on this chart — whether you run an ingress
controller, whether DNS serves a name for it, whether you are willing to put a file on every node.
There is no answer that is right everywhere, so `registry.publish.mode` has no default:

```yaml
registry:
  publish:
    mode: ingress   # ingress | nodePort | external | internalOnly
```

**`ingress`** — the mode that needs nothing on the nodes, because your ingress controller already
has a name your DNS serves and a certificate your nodes already trust. containerd pulls over HTTPS
like it does from anywhere else.

```yaml
registry:
  publish: {mode: ingress}
  host: oci-composer.example.com
  ingress:
    className: nginx
    annotations: {cert-manager.io/cluster-issuer: letsencrypt}
```

**`nodePort`** — no ingress controller required, at the price of one file per node:

```yaml
registry:
  publish: {mode: nodePort}
  host: oci-composer.internal:30500
  service: {type: NodePort, nodePort: 30500}
```

```toml
# /etc/containerd/certs.d/oci-composer.internal:30500/hosts.toml, on every node
[host."http://<node-address>:30500"]
  capabilities = ["pull", "resolve"]
```

**`external`** — you already run a registry; `registry.enabled=false` plus `defaultRegistry.host`
and `defaultRegistry.existingPushSecret`.

**`internalOnly`** — a deliberate statement that nothing outside the cluster pulls these images.
Useful when something else copies them onward. Not a default, because silently producing images no
Pod can pull is the failure this whole mechanism exists to prevent.

### One consequence worth knowing

`registry.host` is substituted only for objects that named **no** repository. An object that sets
`spec.push.repository` explicitly is reported back exactly as written — rewriting a host a tenant
chose would be a lie in the one field a workload reads.

So an explicit repository has to name something **whoever pushes** can resolve, and for an
`ImageBuild` that is a Job inside the cluster. Naming a node-only address there fails with
`no such host` from BuildKit, in a namespace that has no containerd drop-in and no reason to have
one. Either omit `repository` and let the default registry handle both halves, or name the
in-cluster Service.

### A historical note, because it used to be worse

`registry.host` once fed *both* addresses. Setting it to a node-resolvable name pointed the
controllers at a name cluster DNS could not resolve, and every object failed with `no such host`
before publishing anything; leaving it unset produced images no Pod could pull. There was no
correct value. This project's own e2e hit both halves in order, on consecutive runs, and needed a
CoreDNS entry to work around it — an entry that no longer exists, because there is nothing left to
work around.

## Bringing your own

```yaml
registry:
  enabled: false          # installs no zot at all
defaultRegistry:
  host: ghcr.io/me
  existingPushSecret: my-creds
```

Objects that name no repository then publish to `ghcr.io/me/<namespace>/<name>`, using that Secret.
Everything below about retention still applies — it is a property of the registry you run, not of
the one we bundle.

## What the registry has to do

Not every registry can hold up this project's end of the bargain. Two requirements are load-bearing.

**Online garbage collection.** The Registry HTTP API can enumerate repositories, tags and manifests,
but there is **no blob listing endpoint**. A controller can therefore find an unreferenced
*manifest*; it can never discover an unreferenced *blob*, and blobs are the bulk of the bytes. So
blob reclamation has to happen inside the registry. CNCF `distribution` (`registry:2`) collects
offline, with its own documentation advising the registry be read-only during the pass — which makes
it the wrong fit here rather than merely inconvenient.

**Retention driven by recency of use.** The guarantee below is built on the registry keeping what has
been pulled recently. A registry that only expires by age or by tag count cannot express it.

[zot](https://zotregistry.dev) satisfies both, is a single Go binary, and is what the e2e suite runs
against. Harbor also would, at the cost of Postgres, Redis and several components. See
[ADR 0030](adr/0030-a-real-registry-serves-both-kinds.md).

## The guarantee, and your part in it

> An image named by the retained `status.history` of any live `ImageComposition` or `ImageBuild` is
> **never deleted, by anything.**

The controllers hold this up by re-pulling every image a live object still references, on
`--retention-refresh-interval` (default `1h`). The registry keeps what has been pulled recently. That
is the whole mechanism: a lease the object renews, rather than something inferred from a scan.

**Your part is the window.** It must stay much longer than the refresh interval. The relationship is
the guarantee — not either number:

| Refresh interval | Registry window | Margin | |
|---|---|---|---|
| `1h` | `720h` (30 days) | 720× | the default, and comfortable |
| `1h` | `24h` | 24× | workable; an outage of a day loses content |
| `1h` | `2h` | 2× | **not a guarantee** — one slow cycle is data loss |

If you shorten the window, shorten the interval with it.

## A zot configuration that works

**This is what the chart renders**, reproduced here because it is what you would need if you ran zot
yourself, and because four details in it are easy to get silently wrong. Verified against zot
`v2.1.20` by `test/e2e/`; the chart's copy is checked at build time by
`hack/check-bundled-registry.py`.

```json
{
  "distSpecVersion": "1.1.0",
  "storage": {
    "rootDirectory": "/var/lib/registry",
    "gc": true,
    "gcDelay": "1h",
    "gcInterval": "6h",
    "retention": {
      "dryRun": false,
      "policies": [
        {
          "repositories": ["**"],
          "deleteUntagged": true,
          "keepTags": [{ "patterns": [".*"], "pulledWithin": "720h" }],
          "keepUntagged": { "pulledWithin": "720h" }
        }
      ]
    }
  },
  "http": {
    "address": "0.0.0.0",
    "port": "5000",
    "auth": { "htpasswd": { "path": "/etc/zot/htpasswd" } },
    "accessControl": {
      "repositories": {
        "**": {
          "anonymousPolicy": ["read"],
          "policies": [
            { "users": ["kube-oci-composer"], "actions": ["read", "create", "update"] }
          ]
        }
      }
    }
  },
  "extensions": { "search": { "enable": true } }
}
```

Four details in that file are easy to get wrong, and each was got wrong at least once while this was
being built.

**`extensions.search` is not optional.** Pull-recency retention needs the registry to have *recorded*
a pull, and that metadata lives in the search extension's database. Without it `pulledWithin` has
nothing to match on and **every tag expires however often it is fetched** — the exact opposite of
what the config appears to say.

**`keepTags` needs an explicit `patterns`.** zot retains `patterns` AND (`pulledWithin` OR …), so an
entry with no `patterns` matches no tags, and every tag becomes a deletion candidate.

**`keepUntagged` is a separate rule from `keepTags`.** Tagged and untagged manifests are governed
independently, which is why the controllers refresh **both** the digest and every tag. Leaving
`keepUntagged` out deletes exactly the digest-pinned images
[ADR 0010](adr/0010-workloads-reference-digests.md) tells your workloads to reference.

**A repository the policy does not match is not therefore safe.** Retention policies govern
manifests; zot's blob GC is separate and reclaims what nothing references. An **untagged** manifest
references nothing, so a digest-only artifact in an unmatched repository can be collected within
`gcDelay` of being published -- the publish succeeds, and the pull that follows says `not found`.

That is why the shipped policy is `repositories: ["**"]` with `keepUntagged.pulledWithin`: the
refresher's pull is what keeps a digest-only artifact alive, and it only counts where a policy
applies. If you narrow `repositories`, narrow it to something that still covers every repository
the controllers publish to.

This project's own e2e ran into it from the other end -- a composition whose tags were dropped
became untagged, and the image it had just published was gone before a Pod could pull it.

### TLS

`registry.tls.enabled=true` makes zot terminate TLS itself, with the certificate from
`certManager` (recommended if you run it — renewal is the thing the alternative cannot do),
`secret` (you supply a `kubernetes.io/tls` Secret), or `selfSigned` (the chart generates a CA and
distributes it to both controllers and to build Jobs).

It closes threat I7, and it is **off by default** for one specific reason: zot has a single
listener and cannot serve HTTP and HTTPS at once, so enabling it invalidates the containerd drop-in
on every node.

```toml
# before                              # after
[host."http://<node>:30500"]          [host."https://<node>:30500"]
  capabilities = ["pull", "resolve"]    capabilities = ["pull", "resolve"]
                                        ca = "/etc/containerd/certs.d/oci-composer.internal:30500/ca.crt"
```

The `ca` line is only needed in `selfSigned` mode, and the CA is in the
`<release>-kube-oci-composer-registry-tls` Secret under `ca.crt`. Terminating at an ingress instead
avoids all of this — the node talks to the ingress, which already has a certificate it trusts.

**Self-signed certificates do not renew, and the chart refuses to render once one is close to
expiring.** That is deliberate and it is not merely pedantic: an expired certificate stops the
retention refresh, and a refresher that stops running is what lets the registry reclaim images your
workloads are still running — one window later, silently. Rotation is:

```console
kubectl -n <ns> delete secret <release>-kube-oci-composer-registry-tls
helm upgrade ...
kubectl -n <ns> rollout restart deploy    # zot and both controllers read certs once at startup
```

...and every client has to learn the new CA, nodes included. If that is more than you want to do by
hand, use `mode: certManager`. One caveat there: cert-manager renews the Secret in place and zot
keeps serving the old certificate until its pod restarts, because it reads certs once at startup.
Nothing in the chart can notice — it does not own that Secret — so `renewBefore` is set generously
and restarting the pod after a renewal is yours.

**Without TLS, the write credential crosses the network in the clear.** zot authenticates
writes with HTTP Basic, so over plain HTTP the generated password crosses the pod network readable
by anything positioned to watch it — and that credential is the whole of the "only the controllers
can push" guarantee. Pulls are anonymous, so only the write path is exposed. If your cluster's
network is not a boundary you trust, put TLS in front of the registry and remove its host from
`defaultRegistry.insecure`, or point `defaultRegistry.host` at a registry that already has a
certificate. Threat I7.

**A NetworkPolicy ships enabled, and it is a connectivity guarantee rather than a security
control.** Reads are anonymous by design and writes need the password, so restricting which
namespace may connect adds no authority to either rule. What it buys is builds that work: a build
Job runs in its own object's namespace and crosses a namespace boundary to push, and in a
default-deny cluster nothing lets it through.

Two things before you narrow it. The moment any policy selects the registry pod, that pod goes from
"everything allowed" to "only what is listed" — so the shipped policy has to be complete, not
merely correct. And image pulls come from the **kubelet**, in the node's network namespace, which
no pod selector can ever match; that is what `registry.networkPolicy.nodeCIDRs` is for, and leaving
it empty on a cluster where pods and nodes are on different networks turns every pull into a
timeout that looks exactly like a broken registry.

**Anonymous read, authenticated write.** zot enforces this itself; no proxy is needed in front. Only
the controllers get a write identity — give them a `dockerconfigjson` Secret and point
`spec.push.secretRef` at it. The refresh path needs no more than *read*.

## Staying up while nodes move

`registry.readReplicas` runs extra registry pods that serve **pulls**. Pulls are what fails visibly
when a node is drained — `ErrImagePull` on every workload that needs an image while the registry is
being rescheduled — so this is the half worth replicating.

```yaml
registry:
  readReplicas: 2
  persistence: {accessMode: ReadWriteMany}   # or storage.driver: s3
  cache:
    driver: redis                            # required; see below
    redis: {url: redis://redis:6379}
```

**Only one pod ever writes, and that is the design rather than a simplification.** zot serialises
repository writes with an in-process lock: a push is a read-modify-write of the repository's
`index.json`, and nothing coordinates two processes doing it at once. Measured, two instances writing
one repository lost 2–4% of the tags that had returned `201`, and every instance then agreed those
tags were never written. Garbage collection rewrites `index.json` too, so a pod that merely collects
is a second writer — content that was being actively pulled still went missing. The replicas
therefore take no pushes and collect nothing. `test/spike` reproduces all of it in about two minutes,
and [ADR 0041](adr/0041-one-writer-many-readers.md) records it.

What that buys and does not buy:

- **Pulls survive a drain.** Any surviving pod serves every image.
- **Pushes do not.** While the writer is moving, publishing fails and the controllers retry on the
  next reconcile. Nothing is lost — the reconcile is idempotent — but it is not instantaneous.
- **Push throughput is unchanged.** Every push still goes through one process.
- **The single point of failure moves rather than disappears.** It is now the shared store, which
  becomes the availability floor for every pull in the cluster. Worth doing only where that store is
  itself redundant; three registry pods over one non-redundant fileserver is worse than one pod on a
  volume.

**Redis (or Valkey) is required, and it is not a cache.** Valkey is verified — zot reaches it
through go-redis and accepts the same URL. zot's default metadata database is BoltDB, a file one
process opens exclusively, so replicas cannot share it. Giving each its own would be worse than
inconvenient: `extensions.search` records the pull timestamps `pulledWithin` is measured against, so
a refresh landing on one pod would not save an image from the writer's collector, and retention would
delete content that is still in use.

**And it must be persistent.** A Redis restart without persistence loses every pull timestamp, every
image then looks unpulled, and the next GC pass reclaims images your live objects still reference —
on a `gcDelay` fuse, not a retention window. The chart cannot check this. It is the sharpest thing on
this page.

**The chart checks that you asked for `ReadWriteMany`; it cannot check that you got it.**
`accessModes` on a claim is a request. A provisioner without multi-writer support either leaves the
claim `Pending` — loud, and fine — or binds it anyway and backs it per node, which gives you pods
serving different content behind one Service name. Confirm before relying on the replicas:

```
kubectl -n <ns> get pvc <release>-kube-oci-composer-registry
```

Any volume that genuinely supports multiple concurrent writers works. What it must provide is real
multi-writer access, prompt visibility of one client's writes to the others, and working file
locking; implementations differ, and a `ReadWriteMany` label on a StorageClass is not by itself proof
of any of the three.

`accessMode` is **immutable on a bound claim**, so it cannot be changed by `helm upgrade`. Moving an
existing single-pod install onto shared storage means either switching to `storage.driver: s3` and
copying the repositories across with `skopeo sync` or `oras cp` — verifiable before you cut over — or
scaling to zero, setting the PV's reclaim policy to `Retain`, recreating the claim and copying the
data by hand. There is deliberately no in-place version: for `ImageBuild` those bytes are the only
copy.

Two Services exist once this is on. `<release>-registry` is the write endpoint the controllers push
to and selects the writer alone; `<release>-registry-read` selects every serving pod and is what an
Ingress or NodePort exposes. At `readReplicas: 0` they both resolve to the same single pod.

## What you now own

**Storage and backup.** For `ImageComposition` this is a convenience: everything is rebuildable from
its spec. For `ImageBuild` the registry is a **system of record** — a rebuild may not reproduce the
digest ([ADR 0025](adr/0025-dockerfile-builds-as-a-second-kind.md)) — so if you lose the registry's
storage, those images are gone. Back it up.

**Noticing when refreshing stops.** This design fails *unsafe*: if the controllers stop refreshing
for longer than the window, content is deleted. Sustained failure raises a `RetentionDegraded` event
and logs at error level. Alert on it — the alternative to noticing is finding out one window later.

```
kubectl get events -A --field-selector reason=RetentionDegraded
```

**Keeping the two numbers in step.** Nothing enforces the relationship between the registry's window
and the refresh interval; they live in different systems. Write them down together.

**Where the registry runs during maintenance.** Left unplaced, the registry is an ordinary pod at
default priority, so a drain can evict it in the same batch as the workloads that pull from it —
and then those workloads sit in `ErrImagePull` while the only thing that could serve them is itself
being rescheduled. Two things make the gap longer than the move: a ReadWriteOnce volume has to
detach from the cordoned node and reattach elsewhere, and the kubelet backs `ErrImagePull` off
exponentially to about five minutes, so consumers stay broken well after the registry is healthy.

The registry has its own placement keys, deliberately separate from the top-level ones that place
the controllers — the usual reason to steer the registry is that it should *not* be where they are:

```yaml
registry:
  nodeSelector: {kubernetes.io/hostname: storage-1}   # or a label on your infra pool
  priorityClassName: infra                            # decides who gets a node back first
  tolerations: []
  terminationGracePeriodSeconds: 30
```

`priorityClassName` does not prevent an eviction — a drain evicts regardless of priority — but it
decides who is scheduled first when the remaining nodes are tight, which is exactly the situation
during a cordon.

A `PodDisruptionBudget` is rendered too, but **only above one replica**: a floor of one against a
single replica can never be satisfied, so it would block every drain forever with nothing in the
events explaining why.

## One repository per object

`ImageBuild`'s `push.repository` is yours to choose, so several objects can share one. Nothing
breaks — retention operates per digest, and two objects publishing the same digest both keep it
alive. But sharing makes `status.history` and the registry's contents hard to read against each
other, which is exactly what you want to be able to do while diagnosing a missing image.

## If you would rather not run one

`registry.enabled=false` with `defaultRegistry.host` — see *Bringing your own* above. What you cannot
do is publish nowhere: both kinds upload to a registry, and for `ImageBuild` the Job executes in
another pod with no route back to the controller at all.

## Large pushes, and why concurrency makes them slow

zot guards its whole store with **one write lock**. `FinishBlobUpload` holds it across dedupe and
the blob move; `InitRepo` takes it as the first action of a chunk upload, *before reading any of the
body*. So uploads serialise registry-wide — not per repository — and a queued upload is waiting
before it has transferred anything.

That matters because `http.readTimeout` bounds the **entire request, including the body**, so the
clock is running while a handler waits for the lock. Left unset, zot applies 60 seconds, and a push
can die having sent nothing:

```
Put ".../blobs/uploads/<uuid>?digest=sha256:…": use of closed network connection
```

with `read tcp …: i/o timeout` on the registry side at the same moment. It reads like a network or
storage fault and is neither. BuildKit then retries the blob from zero, and each retry queues behind
the next one, so a couple of failing builds sustain it indefinitely.

The chart sets `registry.readTimeout` to `1h` for this reason. It is not a stall timeout — Go cannot
express one — so it is generous on purpose, and the cost is that a dead connection holds a goroutine
until it expires.

**If concurrent pushes are slow**, the lever is `registry.storage.dedupe: false`. Dedupe runs inside
that lock, so removing it shortens the window everything else waits behind. Measured against
zot v2.1.20 with twenty concurrent 1 MB pushes:

| | single upload | twenty concurrent |
|---|---|---|
| `dedupe: true` (default) | 0.37s | 3.9 – 6.1s |
| `dedupe: false` | 0.34s | 1.0 – 3.9s |

It is a partial fix: `InitRepo` takes the same lock, so uploads still serialise, and it costs disk —
a blob shared by several repositories is stored once per repository. That trade is why the default
is left as zot's own.

**To check whether you are hitting this**, push a large blob directly and watch for a reset at a
suspiciously round number:

```sh
head -c 300M /dev/urandom > /tmp/blob
LOC=$(curl -si -X POST http://<registry>:5000/v2/probe/blobs/uploads/ | tr -d '\r' | awk '/^[Ll]ocation:/{print $2}')
curl -s -o /dev/null -w 'http=%{http_code} time=%{time_total}s\n' \
  -X PUT --data-binary @/tmp/blob "<registry>${LOC}&digest=sha256:$(sha256sum /tmp/blob | cut -d' ' -f1)"
```

A failure at 60.0–60.3s, repeatedly, is this. `test/spike/contention_test.go` reproduces it in
Docker.
