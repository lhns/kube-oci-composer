# kube-oci-composer

A Kubernetes controller that assembles OCI artifacts from content-addressed inputs, so workloads
can get files their upstream image does not ship — without a custom image build, a static PV
mounted over the path, or an initContainer that clones something at runtime.

```yaml
apiVersion: oci.lhns.de/v1alpha1
kind: ImageComposition
metadata:
  name: kafka-tiered-storage
  namespace: opentelemetry-gateway
spec:
  interval: 1h
  layers:
    - name: core
      fetch:
        url: https://github.com/.../core-1.1.1.tgz
        digest: sha256:6f2e…
        unpack: tar.gz
      to: /core
    - name: s3
      fetch:
        url: https://github.com/.../s3-1.1.1.tgz
        digest: sha256:9a4c…
        unpack: tar.gz
      to: /s3
  push:
    # No repository: publishes to the bundled registry as <registry>/<namespace>/<name>.
    # Set `repository:` to publish anywhere else.
    tags: [s1a2b3c4d5e6f7890]
```

Mount the result as an image volume:

```yaml
volumes:
  - name: plugins
    image:
      reference: oci.example.com/kafka-tiered-storage:s1a2b3c4d5e6f7890
      pullPolicy: IfNotPresent
```

## Read this first: how a workload names the artifact

A Pod spec cannot read another object's status, so the reference has to be a literal string in git
and something has to keep it right. **The answer here is to let whatever templates the
`ImageComposition` choose the tag, by hashing the build:**

```
{{- $spec := include "kafka.icspec" . }}      {{/* base + layers + config ONLY */}}
{{- $tag  := printf "s%s" (sha256sum $spec | trunc 16) }}

# ImageComposition   push: {tags: [{{ $tag }}]}
# Deployment         image:   oci.example.com/kafka-tiered-storage:{{ $tag }}
```

Both sides derive the tag from one source in one render, so nothing observes anything at runtime —
no extra controllers, no git write-back, no status reading, no lag. Change a layer and both move
together in one commit; the pod template changes, so a rollout happens by itself.

This is safe **because assembly is deterministic**: the output digest is a pure function of
digest-pinned inputs, so a hash of those inputs identifies the output as precisely as the digest
does. A spec-hash tag cannot change meaning without the spec changing — which is what makes
referencing a tag acceptable here, and `pullPolicy: IfNotPresent` correct alongside it.

**`push.onConflict` defaults to `Fail`** and enforces exactly that: the controller will not move
a tag to different content, it fails the build instead. Republishing identical content is always a
no-op, so a steady reconcile loop never trips it. Two other answers are available — `Overwrite` for
a deliberately moving pointer such as `main`, and `Keep` to leave an existing tag alone and publish
nothing, which is usually what you want with a spec-hash tag. The same field, with the same three
values, means the same thing on `ImageBuild`. See [ADR 0029](docs/adr/0029-three-valued-tag-conflict-policy.md).

**`push.tags` is optional.** Omit it and the artifact is published by digest alone, which is all
a workload pinned by Flux image-automation needs.

**Not templating with Helm?** `push.ref` takes a full image reference and uses its *tag*,
ignoring the host and repository. Anything that already rewrites image references can then set it —
kustomize's `images` transformer reaches `spec.volumes[].image.reference` natively and a CRD field
with a `configurations:` fieldSpec, so a single entry retags the artifact and the workload that
consumes it together:

```yaml
images:
  - name: my-artifact                       # matches push.ref AND the workload's reference
    newName: registry.example/my-artifact
    newTag: s1a2b3c4d5e6f7890
```

Worked example: [`docs/examples/spec-hash-tag`](docs/examples/spec-hash-tag/README.md).
Full reasoning and the alternatives: [ADR 0017](docs/adr/0017-updating-the-consumed-digest.md),
[ADR 0010](docs/adr/0010-workloads-reference-digests.md).

## What it does not do

**Composition cannot run a Dockerfile, and that is the point.** It is a pure function of its
inputs, which is what makes the output digest predictable, the reconcile loop convergent, and the
provenance exact rather than scanned. One non-deterministic step would remove all three — for the
whole object, not just the step — which is why building is a separate kind with its own promise
([ADR 0025](docs/adr/0025-dockerfile-builds-as-a-second-kind.md)) and never a layer verb. Anything
needing a compiler is still best served by ordinary CI. See
[ADR 0001](docs/adr/0001-compose-dont-build.md).

**So compile in CI, and bring the image in here.** Concretely:

```bash
# 1. Your existing pipeline builds and pushes, as it already does.
docker build -t ghcr.io/me/app:v1.2.3 . && docker push ghcr.io/me/app:v1.2.3

# 2. Get the digest. --platform matters: a base must name a platform-specific
#    manifest unless spec.platforms is set.
crane digest --platform linux/amd64 ghcr.io/me/app:v1.2.3
```

```yaml
# 3a. As the foundation — layers are reused verbatim, so nothing is re-uploaded.
base:
  ref: ghcr.io/me/app:v1.2.3@sha256:…

# 3b. …or as content at a path, when it is one component among several.
layers:
  - name: app
    image: {ref: ghcr.io/me/app:v1.2.3@sha256:…}
    to: /opt/app
```

Then let whatever templates the spec choose the tag by hashing it, as above, so the workload moves
with it. Keeping the pin fresh is Renovate's job — a `customManager` regex over the `ref` — or Flux
image-automation's. See [ADR 0024](docs/adr/0024-images-as-layer-sources.md).

**`unpack: deb` resolves no dependencies.** It takes a package's payload and nothing else, so a
package needing three others means three more entries, pinned by hand. Nothing here runs
`postinst`, and a package whose files only work afterwards will not work
([ADR 0022](docs/adr/0022-distro-packages-as-layer-sources.md)).

## Where the images go

**Both kinds publish to a registry, and that is the only way an artifact becomes pullable.** The
chart installs one by default — zot, with credentials it generates itself — so this is not a
prerequisite you have to satisfy before installing anything.

An object that names no repository publishes to the bundled registry as:

```
<registry>/<namespace>/<name>
```

Set `push.repository` to publish somewhere else, with `push.secretRef` for its credentials. Whose
credential is used is a security boundary, not a convenience: the operator's own credential is used
**only** for the registry the operator owns. Name your own repository and you supply your own
credential or push anonymously — never the operator's.
([ADR 0034](docs/adr/0034-a-default-registry.md)).

**One thing to get right before a workload can pull.** `status.artifact.ref` is one string that two
different resolvers have to understand: the controllers reach the registry through **cluster DNS**
to push and refresh, and the kubelet reaches it with the **node's** resolver to pull. So
`registry.host` must resolve in both places — an ordinary DNS name does; a name only your nodes know
leaves every object failing with `no such host` before it publishes anything. The chart warns when
it is unset, because the failure otherwise is a successful publish followed by `ErrImagePull`. See
[docs/registry.md](docs/registry.md).

**An `ImageBuild`'s output is the only copy.** It is an *observation*, not a function of its spec
([ADR 0025](docs/adr/0025-dockerfile-builds-as-a-second-kind.md)), so a rebuild may not reproduce
the digest — which makes the registry's storage the thing you back up. A composition is
rebuildable from its spec, so losing it costs a rebuild rather than data.

Earlier versions could also serve artifacts from the controller itself. That is gone; see
[ADR 0035](docs/adr/0035-a-registry-is-the-only-publication-path.md) for why, and `CHANGELOG.md` for
the migration.

### Keeping published images from being deleted

A registry with an expiry policy will reclaim anything it has not seen used recently — including
images your workloads are still running. Both controllers therefore **re-pull every image a live
object still references**, on `--retention-refresh-interval` (default `1h`), which is what tells the
registry they are in use.

Three things about it are worth knowing:

- **It only reads.** No write permission, no delete permission; nothing it does can destroy an image.
- **The ratio is the guarantee**, not either number. The default assumes a registry window of 30
  days, a margin of 720. If you shorten the window, shorten the interval with it.
- **It never rebuilds.** A refresh is a pull, which matters most where a rebuild would produce
  different bytes.

An object **Stalled on a spec error keeps refreshing what it already published** — those images may
be running right now. Sustained failure raises a `RetentionDegraded` event, because this design fails
*unsafe*: the symptom of silence is deletion, one window later. See
[ADR 0031](docs/adr/0031-the-retention-guarantee.md) and [docs/registry.md](docs/registry.md).

## Operational notes

**Reconciling is nearly free.** `status.inputHash` summarises everything that determines the
output, so a reconcile that changes nothing costs one `HEAD` — no fetch, no assembly.

**Old builds are retained, but not forever.** `--keep-builds` (default 10, overridable per object
via `spec.push.history`) decides how many past builds stay tagged. Expiry is the registry's job now;
what the controllers guarantee is that images a live object still references are refreshed and so
never reclaimed. See [ADR 0011](docs/adr/0011-content-tags-expire.md) and
[ADR 0031](docs/adr/0031-the-retention-guarantee.md).

**The layer cache is pluggable.** Disk by default; point `--s3-endpoint` at object storage to keep
it across restarts and reschedules. Credentials come from `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY`, never from flags. This is *input* caching — it makes reconciles cheap and
holds nothing anyone pulls. See [ADR 0014](docs/adr/0014-pluggable-storage.md).

## Install

One chart, one namespace, three components — the composer, the builder, and a registry for them to
publish into. All on by default, so this is a complete, entirely local system:

```console
helm install kube-oci-composer oci://ghcr.io/lhns/charts/kube-oci-composer   --namespace oci-composer --create-namespace   --set registry.host=oci.example.com
```

**`registry.host` is the name your NODES resolve to the bundled registry**, and it is what
`status.artifact.ref` is built from. Controllers push over cluster DNS and need nothing; a Pod
pulling is a different matter, because containerd resolves image references with the node's resolver
and cannot see `.svc.cluster.local`. Expose the registry with an ingress, or a NodePort plus a
`certs.d` drop-in — [docs/registry.md](docs/registry.md) has both.

Turn pieces off as needed:

```console
  --set imageBuild.enabled=false     # no Dockerfile builds, and no Job-creating RBAC
  --set registry.enabled=false       # bring your own registry:
  --set defaultRegistry.host=ghcr.io/me
  --set defaultRegistry.existingPushSecret=my-creds
```

**`imageBuild.enabled` deserves a moment.** Its controller creates Jobs, which is the ability to run
arbitrary containers in that namespace — the reason it used to be a separate chart you opted into.
Turning it off removes the controller *and* its RBAC, so the capability is gone rather than unused.
See [ADR 0033](docs/adr/0033-one-chart-one-namespace.md).

## Status

**v0.1, and honest about its limits.**

Implemented: base images with layer reuse and config inheritance; `fetch`, `configMap`,
`sourceRef`, `image` and `remove` layer verbs; unpacking `tar`, `tar.gz`, `tar.xz`, `tar.zst`,
`tar.bz2`, `zip`, `deb` and single-file `gz`; ownership, modes and archive subpaths; the full OCI config
surface; publication to a bundled or external registry with `secretRef`; the input-hash
short-circuit; a two-tier layer cache with optional S3; multi-architecture output; and a retention
refresher that keeps live objects' images from being reclaimed.

Composed artifacts carry **provenance annotations** naming every source that went into them, which
survive the object that produced them.

Not implemented: SBOM and signing ([ADR 0008](docs/adr/0008-supply-chain.md) is Proposed, not
built — and signing is theatre until something verifies it at admission).

**`ImageComposition` will never run a Dockerfile.** That is the scope line, and everything else
follows from it: the output digest is a pure function of the spec, so reconciling is cheap,
provenance is exact, and nothing privileged runs. See
[ADR 0016](docs/adr/0016-the-scope-line-is-determinism.md).

A **separate** kind that does run one, `ImageBuild`, is in alpha — separate binary, separate
ServiceAccount, separate RBAC, and a deliberately weaker promise: its idempotence is a hash of its inputs
recorded in status, not `output = f(spec)`, and it is not bit-reproducible. It is a second
component you install on purpose, never a layer verb, so `kind: ImageComposition` keeps telling you
exactly which guarantee you have. [ADR 0025](docs/adr/0025-dockerfile-builds-as-a-second-kind.md)
records what that costs, and is candid that the case for it is not yet proven.

It ships in the same chart, enabled by default, and its controller can create Jobs — that is, run
arbitrary containers. `--set imageBuild.enabled=false` removes it and its RBAC together.

```yaml
apiVersion: oci.lhns.de/v1alpha1
kind: ImageBuild
metadata: {name: app}
spec:
  context: {kind: GitRepository, name: app-src}   # digest resolved by source-controller
  dockerfile: Dockerfile
  platforms: [linux/amd64]
  # push is optional: omitted, this publishes to the operator's registry as
  # <default-registry>/<namespace>/app. Name a repository to publish elsewhere, and
  # supply your own secretRef with it — the operator's credential is never sent to a
  # registry an object chose for itself (ADR 0034).
```

Every `FROM` must be pinned by digest, and the build runs rootless in its own Job under a service
account bound to nothing. Anyone who can push to the referenced repository can run code in that
namespace — that is the trade this component makes.

### The shape

```yaml
spec:
  # Optional. Absent = scratch, which is right for a bundle that is only ever mounted.
  base:
    image: quay.io/strimzi/kafka
    digest: sha256:…             # an index only when platforms names more than one
    secretRef: {name: kafka-pull}

  # Optional. Two or more publishes an OCI index with one child per platform; one, or none, is a
  # single manifest. Unset = the base's platform, or the controller's own when there is no base
  # — the one input that is not from the spec (ADR 0002). On a MIXED-architecture cluster, name
  # them here or pin the controller with a nodeSelector, or the same spec can build differently
  # depending on where the leader runs.
  platforms: [linux/amd64, linux/arm64]

  # Ordered. Each entry produces exactly one layer. Later entries overlay earlier ones.
  layers:
    # Fetch over HTTP. The digest is DECLARED, because nothing else addresses an arbitrary URL.
    - name: core
      fetch:
        url: https://.../core-1.1.1.tgz
        digest: sha256:…
        unpack: tar.gz
        subpath: core-1.1.1      # strip a version-named wrapper directory
      to: /plugins
      owner: {uid: 1001, gid: 0}
      mode: {file: "0644", dir: "0755"}

    # An image your CI already built. Its layers are FLATTENED into one — whiteouts applied —
    # so this places files at a path, unlike base, which is a foundation under everything.
    # Config is not inherited from it; only base does that.
    - name: app
      image:
        ref: ghcr.io/me/app:v1.2.3@sha256:…
        subpath: usr/local/bin      # take one directory out of the image
      to: /opt/tools
      mode: {file: "0755"}

    # A zip release, which for a great deal of software is the only one published.
    # A zip records unix permissions only if whoever wrote it did: an archive made on Windows
    # carries none, so every file arrives non-executable and a binary needs the mode below.
    - name: plugin
      fetch:
        url: https://.../plugin-1.2.3.zip
        digest: sha256:…
        unpack: zip
        subpath: plugin-1.2.3
      to: /plugins
      mode: {file: "0755"}

    # A distribution package, when that is the only published build of a native library.
    # unpack: deb takes the payload only — nothing is installed and no maintainer script runs.
    # The .so must match the image it is mounted into; nothing here can check that (ADR 0022).
    - name: lualdap
      fetch:
        url: https://deb.debian.org/debian/pool/main/l/lua-ldap/lua-ldap_1.3.0-2+b1_amd64.deb
        digest: sha256:…
        unpack: deb
        subpath: usr/lib/x86_64-linux-gnu
      to: /

    # A ConfigMap. Each key becomes one file; the digest is RESOLVED by hashing the content, so
    # an edit rebuilds. Keys cannot contain "/", so nested layouts need a sourceRef.
    - name: settings
      configMap: {name: kafka-settings}
      to: /config

    # A Flux source. The digest is RESOLVED from its status.artifact.
    - name: overlay
      sourceRef:
        kind: GitRepository
        name: platform-config
        namespace: flux-system
        subpath: overlays/kafka
      to: /etc/kafka

    # Delete something the base shipped. Takes absolute paths; no to/owner/mode.
    - name: prune
      remove: [/opt/kafka/libs/deprecated.jar]

  config:
    inherit: true                # take the base's entrypoint, env, user, workdir, ports, signal
    user: "1001"
    workingDir: /opt/kafka
    exposedPorts: ["9092/tcp"]
```

Exactly one of `fetch`, `configMap`, `sourceRef` or `remove` per entry, each carrying its own
options. `to` is required for content entries and forbidden for `remove`.

**Layers are contributed in declaration order.** The base is hoisted out of the list because it is
not an ordinary entry — see [ADR 0016](docs/adr/0016-the-scope-line-is-determinism.md).

### Bundle or runnable image

The same artifact format serves both. A composition with no `base` is a bundle of files, mounted
with `spec.volumes[].image` into a container that keeps its upstream image — so an upstream release
changes nothing here. A composition with a `base` and `config.inherit` is a runnable image, used as
the container's `image:` — one artifact, no volume, at the cost of a base digest to bump on every
upstream release.

Note for `configMapGenerator` users: kustomize appends a content hash to the generated name and
rewrites references, but only in fields it knows about — **not** `layers[].configMapRef.name`.
Either add a `nameReference` entry to a kustomize `configurations:` file, or use a fixed-name
ConfigMap, which the controller watches directly.

## Relationship to Flux

This borrows Flux's conventions — `interval`, `suspend`, kstatus conditions,
`observedGeneration`, `status.artifact`, `secretRef`, the reconcile annotation — without depending
on Flux. It runs on any cluster. Where Flux is present, the division is the one Flux already
draws: this controller owns what is **available**, git owns what is **deployed**.

## Documentation

[`docs/adr/`](docs/adr/) records every decision with its rejected alternatives, including several
that were built, measured, and removed. Start with the index.

- [`docs/registry.md`](docs/registry.md) — running a registry: what it has to support, a verified
  zot configuration, and what you own once you have one.
- [`docs/threat-model.md`](docs/threat-model.md) — STRIDE over the whole system, with what is
  mitigated and what is not.
- [`docs/examples/spec-hash-tag/`](docs/examples/spec-hash-tag/) — deriving a tag from a hash of the
  spec, which is what makes referencing a tag safe.

## Licence

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
