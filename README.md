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
  publish:
    name: kafka-tiered-storage
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

# ImageComposition   publish: {name: kafka-tiered-storage, tags: [{{ $tag }}]}
# Deployment         image:   oci.example.com/kafka-tiered-storage:{{ $tag }}
```

Both sides derive the tag from one source in one render, so nothing observes anything at runtime —
no extra controllers, no git write-back, no status reading, no lag. Change a layer and both move
together in one commit; the pod template changes, so a rollout happens by itself.

This is safe **because assembly is deterministic**: the output digest is a pure function of
digest-pinned inputs, so a hash of those inputs identifies the output as precisely as the digest
does. A spec-hash tag cannot change meaning without the spec changing — which is what makes
referencing a tag acceptable here, and `pullPolicy: IfNotPresent` correct alongside it.

**`publish.immutable` defaults to true** and enforces exactly that: the controller will not move a
tag to different content, it fails the build instead. Republishing identical content is always a
no-op, so a steady reconcile loop never trips it. Set it false for a deliberately moving pointer
such as `main`.

**`publish.tags` is optional.** Omit it and the artifact is published by digest alone, which is all
a workload pinned by Flux image-automation needs.

**Not templating with Helm?** `publish.ref` takes a full image reference and uses its *tag*,
ignoring the host and repository. Anything that already rewrites image references can then set it —
kustomize's `images` transformer reaches `spec.volumes[].image.reference` natively and a CRD field
with a `configurations:` fieldSpec, so a single entry retags the artifact and the workload that
consumes it together:

```yaml
images:
  - name: my-artifact                       # matches publish.ref AND the workload's reference
    newName: registry.example/my-artifact
    newTag: s1a2b3c4d5e6f7890
```

Worked example: [`docs/examples/spec-hash-tag`](docs/examples/spec-hash-tag/README.md).
Full reasoning and the alternatives: [ADR 0017](docs/adr/0017-updating-the-consumed-digest.md),
[ADR 0010](docs/adr/0010-workloads-reference-digests.md).

## What it does not do

**It cannot run a Dockerfile, and that is the point.** Composition is a pure function of its
inputs, which is what makes the output digest predictable, the reconcile loop convergent, and the
provenance exact rather than scanned. One non-deterministic step would remove all three. Anything
needing a compiler is served by ordinary CI. See [ADR 0001](docs/adr/0001-compose-dont-build.md).

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

## No registry required

`spec.push` is optional.

- **Omitted** — the controller serves the artifact from its own read-only OCI endpoint. Nothing
  else to install, no registry credentials anywhere. An ordinary Service behind your existing
  ingress and certificate is enough for containerd to pull over HTTPS, with **no node
  configuration** — no `hosts.toml`, no DaemonSet, no containerd socket.
- **Present** — push to an external registry for sharing beyond the cluster.

Determinism is what makes serving mode cheap: nothing here is a system of record, because any lost
artifact can be rebuilt from its spec. See [ADR 0006](docs/adr/0006-push-is-optional.md).

## Operational notes

**Reconciling is nearly free.** `status.inputHash` summarises everything that determines the
output, so a reconcile that changes nothing costs one `HEAD` — no fetch, no assembly.

**Old builds are retained, but not forever.** `--gc-keep-builds` (default 10, overridable per
object via `spec.publish.history`) decides how many past builds stay pullable. Garbage collection
refuses to run at all while any `ImageComposition` has not been reconciled, honours a grace
period, and logs every deletion. See [ADR 0011](docs/adr/0011-content-tags-expire.md).

**Run one replica.** The serving endpoint is leader-election scoped and its blob store is
node-local unless S3-backed. A standby neither reconciles nor listens, and stays out of the
Service because it never reports ready — active/standby, not scale-out.

**Storage is pluggable.** Disk by default; point `--s3-endpoint` at object storage to keep the
layer cache across restarts and reschedules. Credentials come from `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY`, never from flags. See [ADR 0014](docs/adr/0014-pluggable-storage.md).

**Readiness means the store is warm.** After a restart the pod stays out of the Service until its
artifacts have been rebuilt, rather than answering 404 and putting workloads into
`ImagePullBackOff`.

## Install

```console
helm install kube-oci-composer oci://ghcr.io/lhns/charts/kube-oci-composer \
  --namespace oci-composer --create-namespace \
  --set operator.servingHost=oci.example.com
```

`operator.servingHost` is required unless every `ImageComposition` sets `spec.push`: it is what
`status.artifact.ref` is built from, so it must match how the Service is exposed.

## Status

**v0.1, and honest about its limits.**

Implemented: base images with layer reuse and config inheritance; `fetch`, `configMap`,
`sourceRef`, `image` and `remove` layer verbs; unpacking `tar`, `tar.gz`, `tar.xz`, `tar.zst`,
`tar.bz2`, `zip`, `deb` and single-file `gz`; ownership, modes and archive subpaths; the full OCI config
surface; the built-in serving endpoint; external push with `secretRef`; the input-hash
short-circuit; a two-tier layer cache with optional S3; manifest persistence across restarts;
multi-architecture output; and garbage collection.

Not implemented: SBOM, provenance and signing ([ADR 0008](docs/adr/0008-supply-chain.md) is Proposed, not
built — and signing is theatre until something verifies it at admission).

**It will never run a Dockerfile.** That is the scope line, and everything else follows from it:
the output digest is a pure function of the spec, so reconciling is cheap, provenance is exact,
and nothing privileged runs. Compiling source belongs in ordinary CI. See
[ADR 0016](docs/adr/0016-the-scope-line-is-determinism.md).

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

## Licence

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
