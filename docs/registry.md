# Running a registry for kube-oci-composer

Both kinds can publish to a registry you run, and for `ImageBuild` that is the only option. This is
what the operator owns when they do.

If you only compose artifacts and serve them from the built-in endpoint, none of this applies —
see [ADR 0032](adr/0032-the-embedded-registrys-future.md) for why that path is still supported.

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

Verified against zot `v2.1.20` by `test/e2e/`. Adjust the window; keep the shape.

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

Compose-only deployments can use the built-in endpoint and skip all of this
([ADR 0032](adr/0032-the-embedded-registrys-future.md)). If you run `ImageBuild`, you cannot: the
build Job executes in another pod and has no route to the controller's loopback-only write path.
