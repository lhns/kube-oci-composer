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
      url: https://github.com/.../core-1.1.1.tgz
      digest: sha256:6f2e…
      unpack: tar.gz
      target: /core
    - name: s3
      url: https://github.com/.../s3-1.1.1.tgz
      digest: sha256:9a4c…
      unpack: tar.gz
      target: /s3
  publish:
    name: kafka-tiered-storage
    tag: main
```

Mount the result as an image volume:

```yaml
volumes:
  - name: plugins
    image:
      reference: oci.example.com/kafka-tiered-storage:main@sha256:abcd…
      pullPolicy: IfNotPresent
```

## Read this first: reference digests, never tags

**A workload must never reference the moving tag.** Every build publishes two references:

- an **immutable content tag**, `<tag>-<digest[:12]>`, never reused for different content;
- a **moving pointer**, `spec.publish.tag`, repointed at the newest build — for automation to
  watch, never for a workload to name.

A mutable tag plus `pullPolicy: IfNotPresent` means nodes keep stale bytes and you cannot tell
which pod is running which content. That is the one mistake here that fails silently, which is why
it is the first thing in this README.

With Flux, `image-reflector` resolves the pointer and `image-automation` writes the digest into
git behind an `$imagepolicy` marker, so the digest is committed and reviewable. Without it, pin
the content tag by hand — correct, just manual, and bounded by retention (see below).

Full reasoning: [ADR 0010](docs/adr/0010-workloads-reference-digests.md).

## What it does not do

**It cannot run a Dockerfile, and that is the point.** Composition is a pure function of its
inputs, which is what makes the output digest predictable, the reconcile loop convergent, and the
provenance exact rather than scanned. One non-deterministic step would remove all three. Anything
needing a compiler is served by ordinary CI. See [ADR 0001](docs/adr/0001-compose-dont-build.md).

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

Implemented: `url`, `sourceRef` and `configMapRef` layer sources; the built-in serving endpoint;
external push with `secretRef`; the input-hash short-circuit; a two-tier layer cache with optional
S3; manifest persistence across restarts; and garbage collection.

Not implemented: `image` layer sources (the field exists and a spec using it will stall);
multi-architecture; SBOM, provenance and signing ([ADR 0008](docs/adr/0008-supply-chain.md) is
Proposed, not built — and signing is theatre until something verifies it at admission).

### Layer sources

```yaml
layers:
  # A URL, with the digest declared in the spec.
  - name: core
    url: https://github.com/.../core-1.1.1.tgz
    digest: sha256:6f2e…
    unpack: tar.gz
    target: /core

  # Files from a Flux source. No digest here: source-controller has already content-addressed
  # the revision, so the controller RESOLVES it from status.artifact.
  - name: config
    sourceRef:
      kind: GitRepository
      name: k0s-flux
      namespace: flux-system
      path: ./overlays/kafka       # optional; defaults to the whole artifact
    target: /config

  # A ConfigMap. Each key becomes one file; the digest is resolved by hashing the content, so an
  # edit rebuilds. ConfigMap keys cannot contain "/", so nested layouts need a sourceRef.
  - name: settings
    configMapRef:
      name: kafka-settings
    target: /settings
```

**Layers are contributed in declaration order.** No entry type is special or implicitly first.

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
