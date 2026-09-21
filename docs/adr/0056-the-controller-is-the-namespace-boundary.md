# 56. The controller is the namespace boundary

## Status

Accepted. Supersedes [ADR 0055](0055-exporting-a-reference-a-consumer-cannot-compute.md) in part,
reversing two of its decisions rather than refining them.

## Context

ADR 0055 added `push.writeRefTo`, which writes a published reference into a ConfigMap a Flux
`postBuild.substituteFrom` reads. It granted the write as a Role in each allow-listed namespace and
argued the point in these words: *"a cluster-wide grant guarded only by a flag would have made the
code the sole boundary."*

That holds for a foreign namespace. It does not survive two things found since.

### The useful target is usually the object's own namespace

ADR 0055 reasoned about `flux-system` — the namespace that parameterises a cluster — because that is
where a Kustomization substitutes from. But most consumers substitute from their *own* namespace, and
that case cannot be served by the allow-list without listing every tenant namespace as one is added.

RBAC is granted before the object exists. There is no "permit this one namespace, now": to write
wherever an object happens to live, the grant must already exist everywhere. So own-namespace export
is either cluster-wide or it is a list nobody will keep current.

It is also not an escalation. The tenant already owns that namespace, and the builder already creates
Secrets in it — a strictly stronger verb on a strictly more sensitive resource, granted cluster-wide
for exactly the same reason (a Job can only mount Secrets from its own namespace, ADR 0050).

### The refused half of the feature was the half that wanted it

`Push` is shared by both kinds, so `writeRefTo` appears on `ImageComposition` too, where nothing
implemented it — it did nothing, silently. ADR 0055 implies it is unnecessary there, since
[ADR 0017](0017-updating-the-consumed-digest.md)'s spec-hash tag makes a composition's reference
computable in advance.

Computable is not the same as convenient. Computing it means reproducing the spec hash on the
consuming side, which is not trivial and is easy to get subtly wrong — and getting it wrong yields a
reference to an image that was never built, discovered at rollout. That is a usability argument
rather than a correctness one, and it is sufficient.

### A grant the design needed and never had

`builder-rbac.yaml` said the finalizer deletes the ConfigMap on the object's way out. **No `delete`
verb on ConfigMaps was granted anywhere** — not in the ClusterRole, not in the per-namespace Role.
The finalizer would have failed `Forbidden` and blocked deletion of every exporting object. Found
before release, and it is what prompted asking what else the lifecycle did not cover.

## Decision

**The controller enforces which namespace may be written to. RBAC stops being that boundary.**

- `configmaps: [get, list, watch, create, update, delete]` on both ClusterRoles. The per-namespace
  Role is removed: once the ClusterRole carries the verbs it adds nothing, and RBAC that looks like
  a boundary while being inert is worse than no RBAC at all.
- A write is permitted to the object's **own** namespace, or to one the operator allow-listed with
  `--ref-export-namespaces`. Anything else is a terminal spec error. The allow-list keeps its
  meaning and its empty default; it now governs foreign namespaces only.
- A delete touches only a ConfigMap carrying this controller's managed-by label **and** both owner
  labels matching the object.

**Both kinds export.** The composer gains the same flags, the same code path, and a create verb it
has never had — it creates nothing today, which `_helpers.tpl` states as a property of the design,
and that statement changes with this.

**The name is derived, not chosen.** `<kind>-<namespace>-<object name>` —
`imagebuild-team-a-pymods`, `imagecomposition-team-a-base` — with the kind from the object's GVK.
The `name` field is removed. Two objects can no longer ask for the same ConfigMap, so hijack becomes
impossible rather than refused; and because an object's name cannot change, neither can its export's.

**Tenant metadata is allow-listed.** `labels` and `annotations` write onto an object in somebody
else's namespace, so `--ref-export-allowed-labels` and `--ref-export-allowed-annotations` gate which
keys are accepted, empty by default. A key that is not permitted is a terminal error naming it,
never a silent drop — silently dropping metadata is the failure shape ADR 0055 rejects for the watch
label, where the ConfigMap looks right and nothing rolls out.

**The lifecycle is part of this decision, not an implementation detail.** An export outlives the
spec that asked for it, so the controller records what it wrote in `status.refExport` and removes
what it no longer wants: on a namespace change, on the field being removed, and on deletion.

Own-namespace exports carry a controller owner reference and **no finalizer**. A cross-namespace
owner reference is invalid, which is the only reason ADR 0055 needed one; in the now-common case
Kubernetes reclaims the ConfigMap and the object's deletion stops depending on this controller
running. The finalizer is added only for a foreign target.

## Consequences

**Two functions carry the weight the API server used to.** `namespaceAllowed` and `ownedBy`. A bug
in the first writes into a namespace nobody permitted; a bug in the second overwrites another
object's export. They are the security boundary of this feature, and are tested as such — including
the third-namespace case, which no longer has any backstop behind it.

**Cluster-wide ConfigMap `delete` is the largest grant here.** Stated plainly rather than left in a
template: this controller can delete any ConfigMap in the cluster if the code guarding it is wrong.
The alternative was considered — no `delete`, and a stale export lingers until its object dies —
and rejected, because it leaves a Kustomization substituting from a ConfigMap nothing maintains,
which is the failure this feature exists to prevent.

**One ConfigMap per object, never a shared one.** Several objects cannot write into one ConfigMap.
A Kustomization lists several sources in `substituteFrom` instead, and a shared export is precisely
the object two tenants would fight over.

**Allow-listing a namespace still permits every object in the cluster to write into it**, under a
name carrying its kind and source namespace and with only permitted metadata. Narrowing that to
particular source namespaces was considered and not done: it doubles the configuration for a
boundary the naming rule already makes legible.

**Nothing changes for an object that does not set `push.writeRefTo`.** The field is absent by
default; only the ClusterRole verbs exist unconditionally.

## Alternatives rejected

**A list of source namespaces to render Roles into.** Enforced by the API server, which is stronger.
Rejected because it has to be edited every time a tenant is added, and a list that is not kept
current fails closed in a way that looks like the feature being broken.

**Refusing `writeRefTo` on `ImageComposition`**, on the grounds that ADR 0017 makes the reference
computable. It is computable; reproducing the hash on the consuming side is the part people do not
want to do.

**`resourceNames` on the ConfigMap grant.** It does not apply to `create` — the authorizer does not
know the name of an object that does not exist yet — so it cannot constrain the verb that matters.
