# Changelog

Notable changes. Format loosely follows [Keep a Changelog]; this project is pre-1.0 and the API
may change between minor versions.

## [Unreleased]

## [0.5.0] - 2026-08-23

**A registry is now the only publication path, and there is a second kind.**

Two changes account for most of this release. The controller no longer serves artifacts itself —
it pushes them to a registry, and the chart bundles one so that is still a single `helm install`.
And `ImageBuild` joins `ImageComposition`: a second kind, and a second controller, that runs a
Dockerfile.

Everything else follows from those two, or from the gaps they exposed.

**Upgrading from 0.4.0 needs edits.** The `Changed` section below is the list; the short version is
that `spec.publish` becomes `spec.push`, `registry.publish.mode` must be set, and a `sourceRef`
must name a source in its own namespace.

### Added

- **`ImageBuild`, a second kind, in alpha.** It runs a Dockerfile in a rootless BuildKit Job and
  pushes the result. Deliberately a weaker promise than `ImageComposition`: its idempotence is a
  hash of its inputs recorded in status, not `output = f(spec)`, and it is not bit-reproducible —
  so its output is an *observation*, and the registry holding it is a system of record rather than
  a cache ([ADR 0025](docs/adr/0025-dockerfile-builds-as-a-second-kind.md),
  [ADR 0028](docs/adr/0028-the-kind-is-called-imagebuild.md)).

  Separate binary, separate ServiceAccount, separate RBAC. Its controller can create Jobs — that
  is, run arbitrary containers — so `imageBuild.enabled=false` removes the controller **and** its
  RBAC together. Every `FROM` must be digest-pinned, and the build runs under a service account
  bound to nothing.

  Build pods permit privilege escalation and add `SETUID`/`SETGID`, with seccomp and AppArmor
  unconfined. All four were measured as necessary for rootless BuildKit rather than chosen for
  convenience ([ADR 0027](docs/adr/0027-what-rootless-buildkit-actually-needs.md)); the residual
  risk is stated in `docs/threat-model.md` under E1.

- **A bundled registry, enabled by default, with credentials the chart generates.** zot, its
  htpasswd, and the `dockerconfigjson` both controllers push with, all rendered from one password
  that is reused on upgrade rather than rotated. Anonymous read, authenticated write, enforced by
  the registry itself with no proxy in front.

  It exists so that removing the serving endpoint does not turn "one command" into "one command,
  then go and run a registry". Turn it off with `registry.enabled=false` and point
  `defaultRegistry.host` at your own ([ADR 0030](docs/adr/0030-a-real-registry-serves-both-kinds.md)).

- **A default registry, so `push.repository` is optional.** Objects that name no repository publish
  to `<default-registry>/<namespace>/<name>` — configured once by the operator instead of pasted
  into every spec. Namespace-qualified deliberately: one registry is shared cluster-wide, so a bare
  object name would collide the moment two namespaces both contain an `app`, silently, and the tag
  policy would read the collision as a legitimate conflict.

  **The operator's credential goes to the operator's registry and nowhere else**, keyed on the
  HOST. An object may name its own path inside that registry and still authenticate; an object
  naming a different host uses its own `secretRef` or nothing. The credential is named by
  `--default-push-secret` and read from the CONTROLLER's namespace, never the object's. Otherwise
  anyone able to create an
  `ImageComposition` could point it at a host they control and be handed the operator's password
  ([ADR 0034](docs/adr/0034-a-default-registry.md)).

- **The retention guarantee: images a live object still references are never reclaimed.** A
  registry with an expiry policy deletes what it has not seen pulled — including images your
  workloads are running. Both controllers re-pull every image a live object references, on
  `--retention-refresh-interval` (default `1h`).

  It only ever reads: no write permission, no delete permission, so no bug in it can destroy an
  image. **The ratio is the guarantee**, not either number — the default assumes a 30-day window,
  a margin of 720 — and the chart now refuses to install a margin below 24×. An object Stalled on a
  spec error keeps refreshing what it already published, because those images may be running right
  now; sustained failure raises `RetentionDegraded`, because this design fails *unsafe*: the
  symptom of silence is deletion, one window later
  ([ADR 0031](docs/adr/0031-the-retention-guarantee.md)).

- **SBOM, provenance and signing, for both kinds, all off by default**
  ([ADR 0040](docs/adr/0040-the-supply-chain-work-is-worth-building.md), closing ADR 0020).

  `operator.supplyChain` and `imageBuild.supplyChain` (`--sbom`, `--provenance`,
  `--signing-key-secret`) attach an SPDX SBOM, SLSA provenance, and a cosign signature, and record
  what was attached in `status.attestations` so a converged reconcile costs no extra registry
  requests. For an `ImageComposition` the SBOM is **derived** from the digest-pinned inputs
  and is exact rather than scanned; for an `ImageBuild` it comes from BuildKit, because only the
  build can see what it installed.

  Attestations attach as OCI **referrers**; signatures use cosign's `sha256-<hex>.sig` **tag**,
  which amends [ADR 0008](docs/adr/0008-supply-chain.md) for the reason 0008 itself gives — the
  verifiers that exist read that tag, and a signature on a rail nothing reads is a signature
  nothing checks.

  **Signing is inert until something verifies it.** This chart ships no admission policy; example
  Kyverno and policy-controller policies are in `docs/examples/verify`, with the rollout order that
  will not take out a `CronJob` a month later.

  Enabling attestations on an `ImageBuild` **changes its published digest**: BuildKit attaches them
  as extra manifests in an index, so a single-platform build's artifact becomes an index. Existing
  pins keep resolving; anything assuming the digest named a manifest now finds an index. It is in
  the input hash, so it happens once, visibly.

- **Provenance in the artifact itself, not only in status** (threat R1). Composed images carry OCI
  manifest annotations naming every source that went into them — `de.lhns.oci-composer.sources`,
  plus the assembly version and the base digest — so the record survives the object that produced
  it. Annotations rather than config labels, because a label is part of the image config and would
  present provenance as the application's own metadata. Nothing written is time-dependent, so
  `output digest = f(spec)` still holds.

- **TLS on the bundled registry, opt-in** ([ADR 0038](docs/adr/0038-tls-in-the-cluster.md), closing
  threat I7). `registry.tls.enabled=true` makes zot terminate TLS itself, with the certificate from
  cert-manager, a Secret you supply, or a CA the chart generates. The CA reaches both controllers
  via `--registry-ca-file` — additive to the system roots, never replacing them — and, through the
  same short-lived owned Secret the push credential uses, each build Job.

  Without it the generated push password crosses the pod network in an HTTP Basic header, readable
  by anything positioned to watch and leaving no trace in any log — and that credential is the whole
  of the "only the controllers can push" guarantee.

  **Off by default**, because zot has one listener and cannot serve HTTP and HTTPS at once: enabling
  it invalidates the containerd drop-in on every node. `docs/registry.md` has the exact edit.
  Self-signed certificates do not renew, and the chart refuses to render once one is close to
  expiring rather than warning — an expired certificate stops the retention refresh, and that is a
  deletion one window later rather than an outage.

- **A Dockerfile can come from the spec, and a build can have no context**
  ([ADR 0042](docs/adr/0042-content-addressed-not-flux.md)). `spec.dockerfile.inline` puts the recipe
  in the object, so building an upstream project that ships no Dockerfile no longer means forking it
  to add one file and carrying that fork forever. `spec.context` is optional: a Dockerfile that
  only declares a pinned `FROM` and runs commands reads no files, and requiring a context for it
  meant pointing a Flux source at an empty directory.

  `spec.context` is a union rather than a bare Flux reference — `sourceRef`, `fetch` or `image`,
  described below. The rule it enforces is **"every build input is content-addressed"**, which is
  what the old Flux-only restriction's own reasoning actually supported; requiring Flux specifically
  was narrower than the reason it gave.

  `spec.dockerfile` is a union too — `path`, `inline` or `configMapRef` — and carries **no schema
  default**. A structural default is written into the stored object, so a defaulted `path` would
  make every object look like it had set one and the "exactly one of" rule could never fire. The
  effective default (`Dockerfile` at the context root) lives in the controller, the same arrangement
  `Push.OnConflict` already uses for the same reason.

  An unpinned `FROM` in an **inline** Dockerfile **stalls**, because editing this spec is what
  fixes it and the generation change is what wakes the object. One in a Dockerfile that lives in the
  context still retries rather than stalling: the fix is a push to the source, which raises no
  change here.

  `spec.dockerfile.configMapRef` is the modular form: a platform team owns the recipe, an
  application team owns the `ImageBuild`, and one ConfigMap can serve several builds. The ConfigMap
  is **watched**, so an edit rebuilds promptly rather than at the next interval — which for the
  default hour would read as the controller being broken. Its **content** is hashed, not its name or
  resourceVersion, so an edit rebuilds and repointing at an identical copy does not.

  It cannot be pinned, so `--require-pinned-sources` **refuses** it rather than quietly exempting it.
  The builder gains `configmaps: get;list;watch` for the watch, and nothing else: everything it
  writes into a tenant namespace stays a Secret, the Dockerfile copy included.

  `spec.context.fetch` takes the context from an archive at a **declared** digest, for a project
  that publishes releases rather than one you track a branch of. Only archive unpack modes apply —
  a context is a tree, and `none` or `gz` place a single file.

  The build pod's context fetcher is now this operator's own binary
  (`oci-builder fetch-context`) rather than a shell script. **The script verified nothing** — not
  even the Flux artifact digest the controller already held — and it carried a second copy of the
  wrapper-stripping rule that once disagreed with the controller's, so an unpinned `FROM` was
  correctly refused and every build that passed the check then failed inside BuildKit. The fetcher
  verifies the digest **before** unpacking, refuses path traversal, symlinks leaving the tree,
  absolute entries and a `subpath` that matched nothing, and shares one strip rule with the
  controller. `imageBuild.fetcherImage` defaults to the chart's own
  builder image and joins the input hash.

  The builder gains the SSRF dial guard the composer has (`imageBuild.fetchDenyPrivate`), because
  with a fetch context the controller now GETs a **user-supplied** URL to read the Dockerfile. The
  fetch inside the build pod is deliberately unguarded — that pod runs arbitrary code already.

  `spec.context.image` takes the flattened filesystem of a digest-pinned image, applying whiteouts —
  which is how "compose the workdir, then build it" is spelled without putting a build step inside
  `ImageComposition` ([ADR 0042](docs/adr/0042-content-addressed-not-flux.md)). For that kind alone
  the unpinned-`FROM` check runs in the build pod's fetcher rather than in the controller: reading
  one file out of an image controller-side would mean giving a process shared by every namespace
  registry credentials for arbitrary repositories. The guard moves rather than being skipped, and
  the fetcher is our binary running before BuildKit, not user code.

  The Dockerfile's bytes join the input hash and `RecipeVersion` moves to 2. Previously the content
  needed no hashing because it rode inside the content-addressed context tarball — true then, and
  false the moment the recipe can come from anywhere else.

- **Which sources this project resolves, and which it leaves to source-controller**
  ([ADR 0042](docs/adr/0042-content-addressed-not-flux.md)). Two rules, written down once: if the
  spec names the exact content we resolve it, and if a mutable ref has to be tracked over time
  source-controller does; and we only implement a source whose address can be **verified against
  the bytes we got**.

  So **git stays Flux's** — a branch needs tracking, and even a commit-pinned clone cannot be
  verified against the commit without implementing git's object model, while `GitRepository`
  already publishes a content-addressed tarball. HTTP blobs, ConfigMaps and container images are
  ours, as today. **source-controller installs on its own**
  (`flux install --components=source-controller`), which is what makes delegating git cheap rather
  than a reason to run all of Flux.

  **OCI artifacts are ours and are not built yet** — an artifact pinned by digest passes both
  rules, `image` does not cover it, and today consuming one needs Flux for something Flux is not
  needed for. Recorded as a known gap with a decided owner
  ([ADR 0043](docs/adr/0043-an-oci-artifact-is-a-source-we-own.md)), not an oversight.

- **Build pods no longer reach source-controller** ([ADR 0044](docs/adr/0044-the-builder-proxies-flux-sources.md),
  closing threat I11). The builder serves each build its own Flux artifact, against a per-build
  bearer token, and re-resolves the URL from the `ImageBuild` rather than taking it from the
  request — so a token opens exactly one build and steers nowhere.

  It closes a real weakness rather than a connectivity gap. source-controller serves artifacts over
  **plain HTTP with no authentication**, so any pod that can reach it can fetch *any* namespace's
  source. Build pods used to fetch directly, and the NetworkPolicy you had to write per namespace to
  make builds work on a default-deny cluster was itself what granted every tenant's build pod that
  reach. `imageBuild.networkPolicy` now admits build namespaces to the **builder** instead — a pod
  this chart owns, where reaching the endpoint is not reading it.

  A pipe, not a cache: nothing is stored, and the pod still verifies the digest, so a wrong answer
  is caught rather than built. Only `sourceRef` goes through it; `context.fetch` and `context.image`
  are external by nature and stay direct, because proxying an arbitrary user URL would make the
  controller an SSRF amplifier. The cost, stated in the ADR: the controller is now in the data path,
  so a builder that dies mid-stream fails that build.

- **The context fetch is retried**, which fixes builds failing permanently on any default-deny
  cluster. `fetch-context` dials at t=0 of a brand-new pod, every CNI programs NetworkPolicy
  asynchronously *after* the pod has its IP, and the denial arrives as `connection refused` rather
  than a timeout — so it reads like a broken Service. With `backoffLimit: 0` there was no second
  attempt and a transient condition was a permanent failure. Six attempts over about fifteen
  seconds; 4xx and digest mismatches are not retried.

- **A NetworkPolicy for the registry, enabled by default.** Build Jobs run in their object's
  namespace, not the release's, so every build crosses a namespace boundary to push and a
  default-deny cluster blocks it. The policy admits every namespace on the registry port, which is
  deliberate: reads are anonymous by design and writes need the password, so a namespace boundary
  adds no authority. It is a connectivity guarantee, not a security control, and it says so.

- **An Ingress for the registry**, which is the only publish mode needing nothing on the nodes —
  your ingress already has a name your DNS serves and a certificate your nodes trust. That was the
  one genuinely good property of the serving endpoint, and it was never about the endpoint.

- **An `image` layer verb, and `base.ref`.** An image can now be a layer source, flattened into the
  artifact, and a base can be named by a full reference rather than image-plus-digest
  ([ADR 0024](docs/adr/0024-images-as-layer-sources.md)).

- **`unpack: zip`, plus `tar.xz`, `tar.zst`, `tar.bz2` and single-file `gz`.** A great deal of
  published content is not a gzipped tar, and until now none of it could enter an artifact
  ([ADR 0023](docs/adr/0023-more-archive-formats.md)).

- **`onConflict`, a three-valued tag policy**, replacing the two-valued `immutable` on both kinds:
  `Fail` (refuse and stall), `Overwrite` (move the tag deliberately, for a pointer like `main`),
  and `Keep` (leave the existing tag and publish nothing, which is usually what you want with a
  spec-hash tag). A divergence `Keep` left in place is recorded in `status.conflict`, so it is
  visible rather than inferred. `immutable` still works and is deprecated
  ([ADR 0029](docs/adr/0029-three-valued-tag-conflict-policy.md)).

- **`push.history` and `push.ref`.** `history` overrides `--keep-builds` per object, which matters
  most on `ImageBuild`, where the only copy of an output is the one in the registry. `ref` takes a
  full image reference and uses its *tag*, so anything that already rewrites image references —
  kustomize's `images` transformer, for one — can retag the artifact and its consumer together.

- **`sourceRef.revision`** pins the revision a layer expects. Optional by design — a composition
  that tracks a branch is a legitimate thing to want — and `--require-pinned-sources` now lets an
  operator refuse unpinned sources for a whole cluster, on both kinds (threat T1).

- **`status.history[].sources`** records where each layer came from: its name, the resolved digest,
  and the revision. This is what ADR 0026's incident needed and did not have — it had to be
  diagnosed by extracting a layer and reading its payload.

- **SSRF controls on `fetch.url`** ([ADR 0036](docs/adr/0036-ssrf-on-fetch-urls.md), threat I6).
  Link-local addresses are refused unconditionally — `169.254.169.254` is the cloud metadata
  endpoint on every major provider and hands credentials to anything that asks. Other private
  ranges are refused only under `--fetch-deny-private`, because an artifact server on a private
  address is an ordinary layer source and a guard that refuses those is one people turn off.

  Enforced in the dialer, after resolution and before `connect(2)`, so a hostname pointing at the
  metadata IP, a redirect to it, and a DNS rebind are all caught.

- **`--insecure-registry`**, a list of hosts reachable over plain HTTP, matched on host so that
  naming one internal registry does not downgrade every other request.

- **Read replicas for the registry** (`registry.readReplicas`,
  [ADR 0041](docs/adr/0041-one-writer-many-readers.md)). Extra registry pods that serve pulls, so a
  node drain stops taking image pulls down with it. Needs one store every pod can see
  (`persistence.accessMode: ReadWriteMany`, or S3) and a shared metadata database
  (`cache.driver`); the chart refuses to render without both.

  The writer is a **StatefulSet of exactly one**, the readers a separate Deployment, so "one
  writer" is a property of what the chart renders rather than a value somebody can raise. Scaling
  reads is `registry.readReplicas`, which cannot reach the writer.

  **Exactly one pod ever writes**, and that is the design rather than a simplification. zot
  serialises repository writes with an in-process lock, so two instances writing one repository
  lose tags that returned `201` — measured at 2–4%, with every instance then agreeing they were
  never written. Collection rewrites the same index, so a replica that garbage-collects is a second
  writer; content being actively refreshed still went missing. `test/spike` reproduces both in about
  two minutes and is kept as the evidence.

  So: pulls survive a drain, pushes do not — while the writer moves, publishing fails and retries on
  the next reconcile, which is safe because the reconcile is idempotent. Push throughput is
  unchanged. And the single point of failure moves to the shared store rather than disappearing.

- **The registry can be placed, and is no longer evicted alongside its own consumers.**
  `registry.nodeSelector`, `registry.tolerations`, `registry.affinity`,
  `registry.topologySpreadConstraints`, `registry.priorityClassName` and
  `registry.terminationGracePeriodSeconds`. Until now the registry pod spec carried no scheduling
  fields at all — while both controllers honoured the top-level ones — so there was no supported
  way to keep it off the nodes being cordoned. It was rescheduled in the same batch as the
  workloads that pull from it, which then sat in `ErrImagePull` waiting for it.

  Deliberately registry-scoped rather than reusing the top-level keys: the usual reason to steer
  the registry is that it should *not* be where the controllers are, and one shared set of keys
  could not express that.

  A `PodDisruptionBudget` comes with them (`registry.podDisruptionBudget`), rendered **only above
  one replica** — a floor of one against a single replica can never be satisfied, so it would block
  every drain forever with nothing in the events saying why.

### Changed

- **`registry.cluster` never shipped.** It briefly existed on `main` as zot's scale-out mode, which
  **shards** — each repository lived on exactly one member, so a member going down took ~1/N of the
  registry with it. That is throughput, not availability, and availability was what it was reached
  for. `registry.readReplicas` replaces it, and the chart **fails** if `registry.cluster` is still
  set rather than ignoring it, because Helm drops unknown `--set` paths in silence and the result
  would be a quiet scale-down to one pod. [ADR 0039](docs/adr/0039-zot-clustering-is-sharding.md) is
  superseded by [0041](docs/adr/0041-one-writer-many-readers.md).

- **BREAKING: the embedded serving endpoint is removed. A registry is the only publication path**
  ([ADR 0035](docs/adr/0035-a-registry-is-the-only-publication-path.md), superseding ADR 0006).

  `spec.publish` no longer exists. `spec.push` is the only publication block, and it does what
  `publish` did — the two were the same operation to two destinations, and there is one destination
  left:

  ```yaml
  # before                              # after
  publish:                              push:
    name: kafka-tiered-storage            repository: oci.example.com/default/kafka-tiered-storage
    tags: [v1]                            tags: [v1]
  ```

  ...or drop `repository` entirely and publish to the bundled registry.

  **Do this before upgrading, and check.** The chart now upgrades its CRDs with the release, so an
  object still carrying `publish` is rejected loudly — which is the good outcome. The bad one is
  upgrading the controller without the CRD: the object then publishes *nowhere*, reports nothing
  worth noticing, and stops being refreshed. Find them first:

  ```console
  kubectl get imagecomposition -A -o json | jq -r '.items[] | select(.spec.publish) | "\(.metadata.namespace)/\(.metadata.name)"'
  ```

  Everything already published stays where it is; nothing is deleted or re-tagged.

  Removed with it: `internal/serve`, the blob/manifest store, replay and active/standby, and
  `internal/gc`. Flags gone: `--serving-host`, `--serving-bind-address`, `--shared-storage`,
  `--standby-replay-interval`, `--gc-interval`, `--gc-grace`, `--gc-dry-run`,
  `--s3-presign-blobs`, `--storage-backend`, `--storage-dir`. Chart values gone:
  `operator.servingHost`, `operator.servingBindAddress`, `operator.storage.*`, `operator.gc.*`,
  `operator.s3.presignBlobs`, `ingress.*` and the OCI `service.*` block.

  The layer **cache** is untouched: `--cache-dir` and the S3 settings are input caching and have
  nothing to do with serving.

  **What is genuinely lost is node configuration.** The serving endpoint sat behind your ingress
  and certificate, so containerd needed no `hosts.toml`. A bundled registry over a NodePort does.
  Front it with an ingress and a real certificate and that goes away — `publish.mode: ingress`.

- **BREAKING: `registry.publish.mode` is required, and there is no default**
  ([ADR 0037](docs/adr/0037-one-host-cannot-satisfy-two-resolvers.md)).

  `status.artifact.ref` is one string that two resolvers must understand: the controllers reach the
  registry through cluster DNS, the kubelet reaches it with the **node's** resolver. Which public
  path is possible depends on your cluster rather than on this chart, so the chart asks:

  ```yaml
  registry:
    publish:
      mode: ingress        # needs nothing on the nodes
      # mode: nodePort     # one containerd certs.d file per node
      # mode: external     # your own registry
      # mode: internalOnly # nothing outside the cluster pulls these, deliberately
  ```

  `helm install` with no arguments now fails with that message, which is the point: it previously
  succeeded and produced images nothing could pull.

  Internally the two addresses are now separate — `--default-registry` is always the in-cluster
  Service and the public name travels in `--public-registry-host`, reaching `status.artifact.ref`
  and nothing else.

- **BREAKING: a `sourceRef` must name a source in the object's OWN namespace.** Both controllers
  hold cluster-wide read on Flux sources, so without this rule a tenant who can create an
  `ImageComposition` could bake any namespace's content into an image they control and can read.
  It had to be enforced controller-side because a CEL rule cannot read `metadata.namespace`.

- **BREAKING: `--gc-keep-builds` is renamed `--keep-builds`** (`operator.keepBuilds`), which is
  what the builder already called it. The old flag still works and is deprecated: silently dropping
  a flag someone set in a values file becomes a crash-loop on an unknown flag, which is a worse
  upgrade than a rename.

- **`sourceRef` layers hash the artifact's REVISION rather than its tarball digest.**
  source-controller re-packs its artifacts on restart, so the digest changes while the revision it
  describes does not — which rebuilt every composition for bytes that were identical.

- **CRDs install from `templates/` rather than `crds/`.** Helm never upgrades anything in `crds/`,
  which is why schema changes previously needed CRD surgery by hand. Both CRDs carry
  `helm.sh/resource-policy: keep`, so `helm uninstall` cannot take your objects with it.

### Fixed

- **An init-container failure reported nothing actionable.** `jobFailureDetail` iterated only
  `ContainerStatuses`, which does not include init containers, so a build whose context failed to
  fetch said "BackoffLimitExceeded" — the mechanism, with the cause discarded. It reads
  `InitContainerStatuses` too now, init containers first, since one failing means the build container
  never ran.

Only defects that affected 0.4.0. Bugs introduced and fixed within this release cycle are not
listed.

- **The builder could never use an image pull secret.** `builder-deployment.yaml` read
  `.Values.imagePullSecrets`, which is not a key this chart has — so the block silently rendered
  nothing and a private builder image was unpullable with no indication why. It reads
  `image.pullSecrets` now, like the composer, and the registry pod gained the block it never had.

- **A composition could publish a new tag holding the PREVIOUS revision's content, permanently.**
  An artifact whose status predated its own source's spec was consumed as current, and under
  `immutable` the wrong content then held that tag forever — a tag's first publish has nothing for
  the immutability guard to refuse. Sources whose `observedGeneration` lags are now refused as
  Pending rather than consumed ([ADR 0026](docs/adr/0026-a-source-artifact-can-lag-its-own-spec.md)).

- **`flux reconcile` timed out instead of reporting a failure.** `status.lastHandledReconcileAt`
  was never echoed, so Flux waited for an acknowledgement that never came.

- **An archive entry named exactly `..` escaped its target directory.** The traversal guard tested
  for a `../` prefix, which a bare `..` does not have.

- **Entries sharing a name were resolved by an unstable sort**, so two entries with the same name
  could produce different bytes on different runs — in a system whose whole promise is that they
  cannot.

- **Long event messages were dropped by the API server** rather than truncated, so the failures
  worth reading were the ones that vanished.

### Security

- **0.4.0's serving endpoint accepted writes from anything that could reach it.**
  `--serving-bind-address` defaulted to every interface, the chart exposed it as a Service, and the
  Ingress routed `/v2/` including `PUT` — with no authentication. A test confirmed an arbitrary
  pod's `PUT` returned `201 Created`. Both the package documentation and ADR 0025 asserted the
  write path was loopback-only; that was false.

  The endpoint is removed entirely in this release, so the exposure is gone with it. Anyone still
  running 0.4.0 should treat their serving endpoint as writable by anything on the pod network.

## [0.4.0] - 2026-08-14

### Added
- **`unpack: deb`** — a Debian package can be a layer source. For a native library the
  distribution's package is very often the only build that exists, since upstream ships source,
  and until now that content could not enter an artifact at all. Only `data.tar.*` is read, so
  `subpath`, rebasing, mode normalisation, symlink handling and deterministic ordering are the
  same code the tar path uses.

  **Nothing is installed**: no dependency is resolved and no maintainer script runs, so a package
  whose files only work after `postinst` will not work. `unpack: deb` means "this archive has a
  wrapper I know how to remove", exactly as `unpack: tar.gz` means "this archive is compressed".

  The obligation this puts on the caller cannot be checked here: a `.so` must match the image it
  is mounted into, because the artifact is built without reference to its consumer. It fails at
  load time with a soname error rather than half-working. Note also that a distribution's pool
  URL is not permanent — superseded revisions are removed — so a pin eventually 404s, which
  surfaces as a failed fetch and never as wrong content. See
  [ADR 0022](docs/adr/0022-distro-packages-as-layer-sources.md).

  RPM is deliberately not included ([#9](https://github.com/lhns/kube-oci-composer/issues/9)).
  Alpine `.apk` needs nothing: it is already a gzipped tar and works through `unpack: tar.gz`
  with a `subpath`.
- **Per-commit dev builds.** Any branch push now publishes a `sha-<short>` image and a
  `0.0.0-dev.<short>` chart, so a branch can be installed in a real cluster before it is merged.
  Previously the only artifact this project ever produced came from a `v*` tag — the image job in
  CI builds with `push: false` — so testing an unmerged change meant cutting a throwaway release
  tag or building by hand. The chart matters as much as the image, since it carries the CRDs.

  Dev builds are amd64 only and create no GitHub Release. Tag releases are unchanged: multi-arch,
  `latest`, and still gated on the whole ci+e2e pipeline.

### Fixed
- The lint job failed on a timeout rather than a finding. `golangci-lint` gives analysis 1m by
  default and this repo had grown to about that, so the result depended on runner load. Raised to
  5m — the budget is there to catch a hung linter, not to cap how long linting may take.

## [0.3.0] - 2026-08-07

### Added
- **Multi-architecture output** via `spec.platforms`. Two or more entries publish an OCI index
  with one child per platform; one, or none, stays a single manifest. Unset means the base's
  platform, or the controller's own when there is no base -- so on a mixed-architecture cluster,
  name them explicitly or pin the controller, otherwise the same spec can build differently
  depending on where the leader runs. See
  [ADR 0018](docs/adr/0018-multi-architecture-output.md).

## [0.2.2] - 2026-08-05

### Fixed
- **The release pipeline published without being tested.** `release` ran `make test` alone and
  called that a gate; `make test` passes on drifted codegen, an unrenderable chart and a broken
  e2e -- so v0.2.0 and v0.2.1 both published green while the test workflow had been red for a
  week. It now calls the whole `ci` and `e2e` workflows.
- **`ci` and `e2e` cancelled each other** when the release workflow invoked both. Their
  concurrency group was `${{ github.workflow }}`, which under `workflow_call` resolves to the
  *caller* -- so both computed the same group with cancel-in-progress.
- **The e2e image-volume assertion had never actually run.** Fixed, along with the node image
  (its runtime could not mount image volumes), the kind CLI pin, telling containerd where the
  registry is, and the executable bit on the cluster scripts.
- Generated output is now produced by the pinned `controller-gen`, and the pin is a version check
  rather than an existence check -- otherwise a bump reaches CI but never a machine that already
  has an older binary.
- `golang.org/x/text` v0.38.0 -> v0.39.0 (CVE-2026-56852).

## [0.2.1] - 2026-08-05

### Fixed
- A `sourceRef` naming an object that does not exist **yet** no longer stalls the composition.
  Flux objects routinely appear in any order, so "not there" is a normal transient state and has
  to be retryable rather than terminal.

## [0.2.0] - 2026-08-05

### Added
- **A highly available serving endpoint.** With `--shared-storage` (implied by
  `--storage-backend=s3`) every replica serves pulls, instead of one leader serving while standbys
  sit idle. Publishing, garbage collection and status writes stay leader-only, so nothing about
  correctness changes -- serving is read-only.

  This needed two halves: shared blobs, and manifests, which live in the registry's in-memory map
  rather than the store. `StandbyReplay` refills that map on every replica from `status.history`,
  without leader election. Resolves [ADR 0021](docs/adr/0021-active-standby-or-shared-storage.md),
  which spec-hash tags made urgent: every spec change is a new tag and therefore a new pull, so a
  single-point-of-failure registry stopped being acceptable.

  The chart now **fails** on `replicaCount > 1` without shared storage, instead of quietly giving
  you a standby that serves nothing.

### Fixed
- The release job could not create a GitHub Release: it had `contents: read`, so the image and
  chart published and only the release object failed -- which reads like a broken release when the
  artifacts are in fact fine.
- Three lint failures that had CI red on every push.

## [0.1.0] - 2026-08-05

First release. Everything below landed before it, grouped by what it does rather than by the
commit that did it.

### Added
- `ImageComposition` API (`oci.lhns.de/v1alpha1`) with `url` layer sources, deterministic
  assembly, and publication by digest plus any requested tags.
- Built-in read-only OCI serving endpoint, so no registry is required.
- Flux conventions: kstatus conditions, `suspend`, `interval`, `observedGeneration`,
  `status.artifact`, and the `reconcile.fluxcd.io/requestedAt` annotation.
- **Spec-hash tags**, resolving [ADR 0017](docs/adr/0017-updating-the-consumed-digest.md) -- how a
  workload's reference gets updated. The consumer hashes the build-determining part of the spec
  and writes the result into both `publish.tags` and its own image reference, so the two stay in
  step with no image automation, no git write-back and no status reading. Worked example in
  `docs/examples/spec-hash-tag/`.
- **`publish.ref`**, an optional full image reference whose **tag** is added to `publish.tags`; the
  host and repository are parsed and ignored. It lets the tag be set by whatever already rewrites
  image references -- kustomize's `images` transformer, for instance -- so a single entry can retag
  the artifact *and* the workload consuming it, keeping them in step by construction. A ref with no
  tag contributes nothing rather than defaulting to `latest`, so an untemplated manifest degrades
  to digest-only publishing instead of inventing a moving tag.
- `status.inputHash`, so a reconcile that changes nothing costs one `HEAD` instead of
  re-downloading every layer.
- Content-addressed storage with disk, in-memory and S3 backends, and a two-tier layer cache.
- Store-backed blob handler, replacing the upstream disk handler.
- Manifest persistence and replay, so older builds survive a restart.
- Mark-and-sweep garbage collection with retention, a grace period, and a completeness gate.
- `sourceRef` layer sources, reading a Flux GitRepository/OCIRepository/Bucket artifact, with an
  optional subpath.
- `configMapRef` layer sources, with a watch so an edit rebuilds promptly rather than at the next
  interval.
- `image` layer sources: compose over a base image to produce a runnable image rather than a
  bundle. Base layers are reused verbatim, and `config.from` inherits the base's entrypoint, env,
  user, working directory and platform.
- A tested Helm chart, Makefile, Dockerfile and CI; the chart's NodePort can be pinned.
- Architecture decision records in `docs/adr/`.

### Changed
The API was reshaped twice during this release. Recorded because examples and ADRs written before
those changes describe the older shapes:

- **Schema v2**: the base image was hoisted out of the layer list, verbs became source-scoped, and
  removals and ownership were added. Three things made the base untenable as one entry among many
  -- the config had to name which entry it was, it contributes many layers where others contribute
  one, and multi-architecture builds resolve only it per platform. See
  [ADR 0016](docs/adr/0016-the-scope-line-is-determinism.md).
- **`publish.tag` and `push.tag` became `tags`, a list.** Optional, with no default: omit it and
  the artifact is published by digest alone. One build can carry several -- a spec-hash tag
  alongside a readable pointer, or the same hash under more than one algorithm.
- **`immutable` defaults to `true`**, on both `publish` and `push`. It previously existed only on
  `push` and defaulted to false. The controller refuses to move a tag to different content and
  fails the build instead. Republishing *identical* content remains a no-op, so a steady reconcile
  loop never trips it; set `immutable: false` for a deliberately moving pointer such as `main`.
- **The auto-generated `<tag>-<digest[:12]>` content tag is gone**, along with
  `status.artifact.contentTag` (now `status.artifact.tags`) and `BuildRecord.contentTag` (now
  `tags`). It existed only because the tag was a moving pointer and nothing else offered an
  immutable handle; a spec-hash tag is one, and the digest always was.

### Fixed
- Cache returned a path to a file it had already deleted when the remote tier was unavailable.
- Unclosed file handle leaked on every cache miss.
- Disk traversal guard was ineffective on Windows because `filepath.IsAbs` is false for
  `/etc/passwd` there.
- `name.Insecure` was applied to every reference, which would have silently downgraded pushes to
  an external registry to plaintext HTTP.
- `status.artifact.ref` was built from the loopback address rather than the serving host.
- Two API bugs the envtest suite found on its first ever run, and unimplemented image sources
  being accepted rather than rejected.

### Removed
- **`--s3-presign-blobs` and `operator.s3.presignBlobs`.** Presigning existed so the serving
  endpoint could redirect a blob pull straight to object storage. That endpoint is gone (ADR 0035),
  and the flag had been left behind doing nothing but validating itself. `store.Presigner` and the
  `blobs` and `manifests` key namespaces go with it -- the layer cache only ever used `inputs`.


- Pod-reference protection in the garbage collector: implemented, measured to protect nothing, and
  removed. See [ADR 0011](docs/adr/0011-content-tags-expire.md).

[Keep a Changelog]: https://keepachangelog.com/en/1.1.0/
