# Exporting a reference into a ConfigMap

`push.writeRefTo` writes what was published into a ConfigMap, for a consumer that substitutes it —
typically a Flux `Kustomization` with `postBuild.substituteFrom`.

**Read [the naming section in the README](../README.md#read-this-first-how-a-workload-names-the-artifact)
first.** Hashing the spec into a tag is the recommended approach and this page is not a replacement
for it. This exists for the cases that approach does not reach, and it costs something real.

## When you need it

**`ImageBuild`, always, if a workload consumes the result.** A build's digest is an *observation*,
not a function of its spec ([ADR 0025](adr/0025-dockerfile-builds-as-a-second-kind.md)) — the same
inputs can produce different bytes. Nothing renderable in git can name what a build will produce, so
either the build publishes a tag and something keeps the tag honest, or it publishes by digest and
hands you the digest. This is the second.

**`ImageComposition`, if computing the tag is worse than reading it.** A composition's output *is* a
function of its spec, so the spec-hash tag works — but only if you reproduce that hash on the
consuming side, and getting it subtly wrong produces a reference to an image that was never built,
discovered at rollout. The export is available on both kinds for that reason.

## What it costs

Stated plainly, because it is a real trade and
[ADR 0055](adr/0055-exporting-a-reference-a-consumer-cannot-compute.md) rejected it for the kind
where it is avoidable:

- **The digest becomes state outside git.** A revert commit no longer reverts the running image. If
  auditability through git is why you chose this project, that is the property you are giving up.
- **Manifests stop being fully resolvable offline.** A substituted digest is not in the repo, so
  golden-output tests and any `grep '\${'` guard need a documented exemption.
- **A missing key substitutes the empty string, silently.** Flux fails a reconcile for a missing
  *object* but not a missing *key*. The controller never writes a partial reference, which is what
  makes that survivable — but it is why the ConfigMap is replaced wholesale rather than patched.

Keep it to the objects that need it.

## Using it

```yaml
spec:
  push:
    repository: registry.example.com/team-a/pymods
    # No tags at all: publish by digest. Nothing is named, so no name can be remeaned.
    writeRefTo:
      namespace: flux-system
      keys:
        ref: PYMODS_REF        # registry/repo@sha256:...  <- prefer this
        digest: PYMODS_DIGEST  # sha256:...
```

**The ConfigMap's name is derived, not chosen:** `<kind>-<namespace>-<object name>`. An `ImageBuild`
called `pymods` in `team-a` writes `imagebuild-team-a-pymods`. That is the name your Kustomization
must spell:

```yaml
postBuild:
  substituteFrom:
    - kind: ConfigMap
      name: imagebuild-team-a-pymods
```

**Prefer `ref` over `digest` alone.** A consumer substituting a bare digest into an image field
produces a trailing `@` on an empty string if the key is ever missing, which fails later and less
legibly than a missing image would.

## What the operator controls

Nothing here is on by default, and a tenant cannot grant themselves any of it.

| | who sets it | what it does |
|---|---|---|
| `push.writeRefTo.namespace` | the tenant | *asks* for a namespace |
| `refExport.namespaces` | the operator | *permits* a namespace other than the object's own |
| `refExport.labels` | the operator | labels added to every generated ConfigMap |
| `refExport.allowedLabels` / `allowedAnnotations` | the operator | which keys a tenant may set |

**An object may always export into its own namespace.** Anywhere else has to be allow-listed:

```yaml
refExport:
  namespaces: [flux-system]
  labels: "reconcile.fluxcd.io/watch=Enabled"
```

**Allow-listing a namespace permits every object in the cluster to create a ConfigMap there**, under
a name carrying its own kind and namespace — so nothing can collide with or overwrite anything else,
but they can all write. `flux-system` parameterises a whole cluster, so treat listing it as the
privilege it is. [ADR 0056](adr/0056-the-controller-is-the-namespace-boundary.md) records why the
controller enforces this rather than RBAC, and what that costs.

## Making a consumer notice

A ConfigMap change does **not** trigger a Flux reconcile by default. Without a marker, a new digest
is picked up at the next interval and nothing says why.

Flux wants `reconcile.fluxcd.io/watch: Enabled`, and it must be a **label** — kustomize-controller
selects these with `--watch-configs-label-selector`, and a label selector cannot match an annotation.
Written as an annotation the feature is inert in its most dangerous way: the ConfigMap looks correct
and nothing rolls out.

The controller adds no marker unless you configure one, because this writes onto an object in
somebody else's namespace, which is further than borrowing a convention goes. Set
`refExport.labels` as above.

## Lifecycle

- Written **only after a confirmed publish**, and all keys or none.
- Replaced **wholesale**, so a consumer never sees one key updated while another is stale.
- A ConfigMap this controller did not create for *this* object is **never** taken over.
- Moving `writeRefTo.namespace`, or removing the field, **removes** the ConfigMap it wrote — it is
  not left behind for a consumer to go on substituting from.
- Deleting the object removes its export. An export in the object's own namespace is
  owner-referenced, so Kubernetes reclaims it; only a cross-namespace one needs a finalizer, since
  a cross-namespace owner reference is invalid.

## The alternative worth knowing about

Flux's own `ImagePolicy` and `ImageUpdateAutomation` solve the same problem, and their decisive
advantage is that they write the value **back into git** — so the repository stays the record of
what is deployed. If that matters more to you than avoiding a second controller, use them instead.
