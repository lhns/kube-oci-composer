# The registry

Both kinds publish to a registry. **The chart installs one by default** — zot, with credentials it
generates — so a default install is a complete local system with nothing external to configure.

This page is what the operator owns: the one setting that is not automatic, the configuration that
is easy to get silently wrong, and what happens if you bring your own registry instead.

## The one thing that is not automatic

`registry.host`. Everything else has a working default.

**One name has to work from two places.** `status.artifact.ref` holds a single string, and two
different resolvers have to make sense of it:

| Who | Resolves with | Needs it for |
|---|---|---|
| The controllers | **cluster DNS** | pushing, and refreshing to keep images alive |
| The kubelet | the **node's** resolver | pulling, for every workload |

containerd resolves image references with the node's resolver, which does not see cluster DNS, so a
Pod cannot pull from a `.svc.cluster.local` name however healthy the Service is — the image
publishes successfully and then fails with `ErrImagePull`. That much is the well-known half.

The half that is easy to miss: setting `registry.host` to a name only the *nodes* resolve breaks the
other direction. The controllers then cannot reach the name they publish under, and every object
fails with `no such host` from the cluster's DNS server before anything is published at all. This
project's own e2e suite made exactly that mistake, gave the nodes a `hosts.toml` and cluster DNS
nothing, and failed every composition.

**So `registry.host` must resolve in BOTH places.** With an ordinary DNS name that is automatic. If
you use a name only your nodes know, add the matching answer inside the cluster too — a CoreDNS
`hosts` entry pointing at the registry Service is enough.

Two ways to make the node half resolve:

**An ingress**, if you already run one with a certificate.

**A NodePort plus a containerd drop-in**, which needs neither:

```yaml
registry:
  host: oci.internal
  service:
    type: NodePort
    nodePort: 30500
```

```toml
# /etc/containerd/certs.d/oci.internal/hosts.toml, on every node
[host."http://<node-address>:30500"]
  capabilities = ["pull", "resolve"]
```

```yaml
# ...and, in the CoreDNS ConfigMap, the cluster half of the same name:
#     hosts {
#         <registry Service ClusterIP> oci.internal
#         fallthrough
#     }
```

The chart warns at install time when `registry.host` is unset, because the failure otherwise shows up
as a pull error against a Service that looks perfectly healthy.

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

**The bundled registry terminates no TLS, and that has a cost worth stating.** zot authenticates
writes with HTTP Basic, so over plain HTTP the generated password crosses the pod network readable
by anything positioned to watch it — and that credential is the whole of the "only the controllers
can push" guarantee. Pulls are anonymous, so only the write path is exposed. If your cluster's
network is not a boundary you trust, put TLS in front of the registry and remove its host from
`defaultRegistry.insecure`, or point `defaultRegistry.host` at a registry that already has a
certificate. Threat I7.

**Anonymous read, authenticated write.** zot enforces this itself; no proxy is needed in front. Only
the controllers get a write identity — give them a `dockerconfigjson` Secret and point
`spec.push.secretRef` at it. The refresh path needs no more than *read*.

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

## One repository per object

`ImageBuild`'s `push.repository` is yours to choose, so several objects can share one. Nothing
breaks — retention operates per digest, and two objects publishing the same digest both keep it
alive. But sharing makes `status.history` and the registry's contents hard to read against each
other, which is exactly what you want to be able to do while diagnosing a missing image.

## If you would rather not run one

`registry.enabled=false` with `defaultRegistry.host` — see *Bringing your own* above. What you cannot
do is publish nowhere: both kinds upload to a registry, and for `ImageBuild` the Job executes in
another pod with no route back to the controller at all.
