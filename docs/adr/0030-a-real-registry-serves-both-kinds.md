# 30. A real registry serves both kinds, and zot is the one we ship

Date: 2026-08-19

## Status

Accepted.

Amends [0006](0006-push-is-optional.md): push stops being the optional extra and becomes the
recommended shape for anyone running builds. Makes [0013](0013-persist-manifests.md),
[0014](0014-pluggable-storage.md) and [0021](0021-active-standby-or-shared-storage.md) **moot on
that path** — they remain the record for the embedded endpoint, whose future
[0032](0032-the-embedded-registrys-future.md) decides.

Also records, as a correction rather than an edit, that
[0025](0025-dockerfile-builds-as-a-second-kind.md):87-90 rested on a false premise.

## Context

The question that started this was narrower: `ImageBuild` pushes to an external registry and does
nothing else — it does not serve, and it has no retention — so how do you look at a composition and
a build through one endpoint?

The first answer was to give `ImageBuild` its own serving stack, or an `ImageIngress` presenting a
consolidated view over several registries. Working through it, a simpler one emerged: **let a
standard registry be the serving surface for both kinds.** Both controllers already push. The
composer's `spec.push` path already bypasses the embedded registry entirely. The only thing missing
was retention.

That collapses several open problems at once — no `ImageIngress`, no serving stack for the builder,
no shared custom store, no OCI-layout ingest — and `status.history` loses its replay role, because
the registry persists tags itself.

**It is also worth stating plainly that the embedded registry has been the largest single source of
defects here.** Permanent per-replica tag divergence, a Ready-but-empty replica, 416 on resumed
pulls, and an unauthenticated write path were all in that stack. None of them exist when a real
registry serves the images.

### The correction to 0025

0025:87-90 says a build Job "cannot write to the controller's loopback-only serving endpoint". It
could. `--serving-bind-address` defaulted to `:5000` — every interface — the chart exposed it as a
Service, the Ingress routed `/v2/` (a prefix that includes `PUT`), and there was no authentication
anywhere in the package. Anything able to reach the Service could push a manifest or repoint a tag.

That is fixed, by refusing non-idempotent methods from anything but loopback. The premise is
recorded as false here rather than edited into 0025, because a record is evidence of what was
believed when it was written.

## Decision

### The registry is the serving surface, and it is optional to us but real

Both kinds publish to a registry the operator runs. **The chart can install one**, off by default,
so `helm install` still leaves a working system and "a registry becomes required" stops being a real
cost. The embedded endpoint is not removed.

- **Writes:** only the two controllers, via the `dockerconfigjson` Secret they already support.
- **Reads:** anonymous, cluster-wide.
- **Enforced by the registry, not by the network.** No proxy in front.

Not-yet-built images are not a problem: the kubelet retries with backoff until the tag exists.

### zot, not `distribution`, and the reason is garbage collection

**CNCF `distribution` (`registry:2`) is the wrong fit despite being the standard.** Its
`garbage-collect` is an offline mark-and-sweep whose own documentation advises running the registry
read-only during the pass, because a concurrent push can have its blobs swept. It has **no retention
or expiry at all**, and deleting a manifest only untags it — the blobs wait for that offline pass.

**zot** is a single Go binary, OCI-native, with **online garbage collection** and retention policies
expressed over tag count and push/pull recency. Small enough to bundle without becoming its own
operational project.

**Harbor** does all of it plus RBAC, replication and scanning, at the cost of Postgres, Redis and
several components — the opposite of simple.

**Why online collection is a requirement rather than a preference.** The Registry HTTP API can
enumerate repositories, tags and manifests, but there is **no blob listing endpoint**. So a
controller can find an unreferenced *manifest* and delete it; it can never discover an unreferenced
*blob*, and blobs are the bulk of the bytes. Blob reclamation therefore has exactly three homes: the
registry itself (the only clean one), a controller given direct access to the registry's storage
layout (couples to an unstable internal layout — strictly worse than the store we already have), or
the scale-to-read-only CronJob dance `distribution` forces.

**The division of labour: the controller decides what is unreferenced, the registry decides when the
bytes go.**

### zot enforces anonymous-read and authenticated-write by itself

`http.auth.htpasswd` authenticates; `http.accessControl` authorises, with `anonymousPolicy`
separate from the policies that apply to authenticated users. Anonymous gets `read`; the controller
identity gets `create`/`update`. This is a stronger guarantee than the loopback check that protects
the embedded endpoint, because it is the registry's own answer rather than a property of where a
connection came from.

## Consequences

**Bundling means owning storage and backup.** Tolerable for `ImageComposition`, which is
deterministic and rebuildable. For `ImageBuild` the registry becomes a **system of record** —
exactly what [0025](0025-dockerfile-builds-as-a-second-kind.md):105-112 warns about — because a
rebuild may not reproduce the digest. An operator running builds must back up the registry's
storage; nothing else holds those bytes.

**`status.history` loses its replay role.** It exists today because manifests live in
go-containerregistry's unexported in-memory map, so `Blobs`/`Manifests`/`Tags` reconstruct what a
registry would already know. On the registry path history reduces to a retention and audit record.

**The collector stays; the store goes.** What disappears on this path is the store, `NamespaceBlobs`,
`NamespaceManifests`, replay and standby. What remains is the *marking* — walking every live object
of both kinds to decide what must be kept — which is what
[0031](0031-the-retention-guarantee.md), the retention guarantee, is built on.

**One repository per object is documented, not enforced.** `ImageBuild`'s `push.repository` is
user-chosen, so several objects may share one. Nothing breaks — retention operates per digest — but
sharing makes `status.history` and the registry's contents harder to read against each other.

**The README's headline changes.** "No registry to run" becomes one of two supported shapes:
embedded for a small cluster that only composes, a bundled zot once builds are in play. Rewriting it
honestly is part of this change rather than a follow-up.
