# Changelog

Notable changes. Format loosely follows [Keep a Changelog]; this project is pre-1.0 and the API
may change between minor versions.

## [Unreleased]

### Added
- **`onConflict` on both kinds**, replacing the two-valued `immutable` with `Fail` (refuse and
  stall, the default), `Overwrite` (move the tag) and `Keep` (leave it, publish nothing, report
  Ready).

  Two values were the wrong shape for the pattern this project actually recommends. With a tag
  derived from a hash of the spec, a tag that already exists means the content is *already published
  and correct*: refusing stalls over a non-problem, and overwriting rewrites bytes that were already
  right. `Keep` is the missing third answer.

  `Keep` records what it kept in `status.conflict` -- the tag, what it resolves to, what was
  dropped, and when. Without that the object reads Ready while not having published what its spec
  produces, and nothing anywhere says so, which is the shape of the incident behind ADR 0026. The
  record is cleared as soon as a reconcile publishes cleanly.

  `immutable` still works and is honoured when `onConflict` is unset (`true` is `Fail`, `false` is
  `Overwrite`), so existing charts need no edit. Setting both to contradictory values is refused at
  admission; setting both to values that agree is fine. See ADR 0029.

- **`push.history` and `push.ref` on `ImageBuild`**, closing the two gaps that mattered most in the
  drift between `publish` and `push`.

  `history` caps how many past builds are retained. It had existed only on `publish` -- that is, only
  on the kind that can rebuild any artifact from its spec, where retention is a convenience. An
  `ImageBuild` cannot (ADR 0025), so retention there is how much of the ONLY copy is kept, and that
  was the kind without the knob.

  `ref` appends a reference's TAG to the tag list, which is what makes the documented spec-hash tag
  pattern usable from a Helm chart or a kustomize images transformer. Without it the pattern could
  not be used from `ImageBuild` at all -- the place it is wanted most, since a build's tag is the
  only thing identifying which inputs produced it.

- **`status.history[].sources` is now populated for builds.** The field was in the CRD for both kinds
  and only ever written by the composer, so a build record carried a digest with no way to learn
  which revision produced it. That is the exact question ADR 0026's incident was stuck on. Each
  record now names the build context, its resolved revision and its artifact digest.

  The revision is deliberately not part of the input hash: the digest already identifies the content,
  so hashing both would rebuild on a repack that changed nothing. A test holds that line.

- **A parity guard over `Publish` and `Push`** (`kindparity_test.go`). Both divergences above were
  oversights rather than decisions, and they survived because nothing compared the two structs. The
  guard reads both and fails on any field present in one and not the other, unless the difference is
  recorded with the reason the destination makes it meaningless. Verified to fail on drift.

### Changed
- **`--gc-keep-builds` is renamed `--keep-builds`**, which is what the builder already called it. The
  `gc-` prefix was misleading: the flag caps `status.history`, and collection merely honours that cap.
  The old name keeps working as a deprecated alias, so a values file written against the previous
  release does not become a crash-loop on an unknown flag. Setting both takes the new one.

- Schema parity fixes with no behaviour change: `platforms` items are length-limited on both kinds,
  `buildSecret.secretRef` is a pointer like every other `secretRef`, and the status printcolumn
  carries `priority=1` on both so `kubectl get` renders them alike. `SourceRefSource` and
  `RevisionMatches` moved to `shared_types.go`, where both kinds' shared API surface lives.

### Fixed
- **`push.immutable` did nothing on `ImageBuild`.** It was in the CRD from the day that kind
  shipped, defaulted to `true`, and nothing in the build controller read it -- BuildKit pushed over
  whatever the tag held. Anyone who set it, or who simply accepted the default, believed a tag could
  not be silently remeaned, and it could.

  The check now runs **before the Job is created**, which is the whole substance of the fix: BuildKit
  pushes from inside the Job, so a conflict noticed afterwards is a conflict that has already
  happened.

  **This changes behaviour on upgrade.** An `ImageBuild` whose tags already hold content it did not
  publish now goes `Stalled` instead of quietly overwriting. If that is deliberate, set
  `onConflict: Overwrite`; if the content is already correct, `onConflict: Keep`.

### Added
- **Tests for the class of mistake that produced most of this release's bugs**: an assertion written
  alongside the code, describing what it does rather than what it should do. Three shipped that way
  -- a security context demanding a value that stops BuildKit starting, a test asserting a failed
  Job was deleted while that was the bug, and a CI check asserting the broken chart config must
  render. All three passed continuously.

  They share a shape: each was a claim about how an EXTERNAL system reacts -- the kernel, watch
  delivery, helm at install time -- in an environment that could not falsify it. So an assertion
  about an environment the test does not have is a belief, not a test. Two mechanisms follow.

  **envtest for `internal/buildcontroller`.** The fake client cannot deliver watch events, which is
  exactly why the hot retry loop was invisible: deleting an owned Job woke the controller through
  its own watch. A real manager makes the watch real, and the assertion is a rate. Reinstating the
  bug produces 251 reconciles in 15 seconds against a tolerance of 12.

  **Parity tests across the two kinds** (`internal/controller/kindparity_test.go`), applying
  `unpackparity_test.go`'s argument to the controllers: a property held in two hand-maintained places
  needs something that reads both. They cover the namespace scope, the revision pin, event recording
  and the shared helpers -- each verified to fail when one kind drifts from the other.

  The `GitRepository` stand-in moved to `test/crds/` so the e2e cluster and envtest load the same
  file rather than keeping two.

- **An envtest suite for the builder, and parity tests across the two kinds** -- both aimed at one
  failure mode: a test that passes while being wrong about what it checks. Three of those shipped
  this cycle.

  The fake client cannot deliver WATCH events, so a reconcile provoked by the controller's own
  writes is invisible to the unit suite. That is exactly how the retry hot loop survived: deleting a
  failed Job woke the controller through its own `Owns()` watch, the backoff never applied, and
  every unit test passed. `TestAFailingBuildDoesNotSpinTheQueue` runs a real manager and asserts a
  RATE -- with the defect reinstated it observes **251 reconciles in 15 seconds** against a
  tolerance of 12.

  The parity tests apply `unpackparity_test.go`'s argument to the controllers: a property held in
  two hand-maintained places needs something that reads both. They check that both kinds scope
  references to their own namespace, both honour a pinned revision, neither keeps a private copy of
  the shared helpers, and both record events through the helper that applies the length limit --
  the last being a difference that already shipped.

  The `GitRepository` stand-in moved to `test/crds/` so the e2e cluster and envtest load the same
  one; envtest loads CRDs by directory, so a directory of only CRDs is what makes it shareable.

- **`sourceRef.revision` pins the revision a layer expects.** Optional; unset keeps today's
  behaviour of consuming whatever the source publishes.

  It is the only way to make a `sourceRef` layer a pure function of the spec. A branch or a semver
  range moves with no edit to anything, so the staleness check in
  [ADR 0026](docs/adr/0026-a-source-artifact-can-lag-its-own-spec.md) cannot see it — that compares
  `generation` against `observedGeneration`, which is the source reporting on itself. This is the
  assertion from the consuming side, and holds even if that bookkeeping is wrong.

  Matched against Flux's `<ref>@<algo>:<hash>` by whichever half you give: `v0.6.8` matches
  `v0.6.8@sha1:<anything>`, `v0.6.8@sha1:b739efb5` matches only that commit. The short form exists
  because a generator usually knows the tag it asked for and not the commit it resolved to, and the
  check should not require the half it cannot supply.

  A mismatch is **pending, not stalled**: what fixes it is the source catching up, which raises no
  generation bump here, so stalling would wait for an event that cannot come. Honoured by
  `DockerBuild.spec.context` too.

- **`status.history[].sources` records where each layer came from** — its name, the resolved digest,
  and the revision for a source that has one. Answering "which commit is in this image?" previously
  meant pulling the manifest, fetching the layer and reading its payload, which is exactly how the
  incident behind [ADR 0026](docs/adr/0026-a-source-artifact-can-lag-its-own-spec.md) had to be
  diagnosed, and why a wrong artifact sat unnoticed until a tag conflict surfaced it.

- **`--insecure-registry`** on the builder: registry hosts to push to over plain HTTP,
  comma-separated. Opt-in **per host** rather than a global switch -- an internal or air-gapped
  registry without TLS is a real deployment, but naming one must not quietly downgrade every other
  push the same controller makes. Deliberately not part of the build input hash: how bytes are
  transported does not change what they are, so flipping it rebuilds nothing.
- **`DockerBuild` (alpha).** The kind ADR 0025 describes, now implemented: a second controller,
  a second binary (`cmd/oci-builder`), a second chart (`charts/kube-oci-builder`) carrying its own
  CRD, and its own RBAC.

  **Installing the composer does not install this.** That is the whole shape of the thing. The
  composer's role cannot create a single object; this one creates Jobs, which is the ability to run
  arbitrary containers, and ADR 0004 rejected a feature flag because "a flag set to `false` is a
  weaker guarantee than a component that does not exist".

  How it works: the context comes from a Flux source, so its digest is resolved rather than
  declared; that digest, the spec, and **the pinned digests of BuildKit and the Dockerfile
  frontend** form an input hash. An unchanged hash skips the build entirely, which answers ADR
  0001's objection that the loop "would have to rebuild to discover whether a rebuild was needed".
  The builder digests are hashed because for this kind the algorithm is not in this binary; the
  controller **refuses to start** if either is unpinned, and the chart refuses to render.

  Each build is one Kubernetes Job, rootless, in the object's own namespace, under a service
  account that is deliberately bound to nothing — a pod running code from a git repository must not
  carry the token of the thing that created it. Privileged is not offered at any setting. The Job
  name is derived from the input hash, so a controller restart adopts the running build instead of
  starting a second one. Secrets are passed via BuildKit's secret mount, never as build args, and
  only their `name`/`resourceVersion` reach the input hash — a hash of a low-entropy secret in a
  world-readable status field would be an oracle. The build cache is always per-object; a shared
  one is a channel between whoever can write their Dockerfiles.

  A floating `FROM` is refused before a Job is created. It is the single largest source of "same
  commit, different image", and it is the one rule about a Dockerfile's *content* this controller
  enforces.

  **Read [ADR 0025](docs/adr/0025-dockerfile-builds-as-a-second-kind.md) before using it.** The
  promise is deliberately weaker: the output digest is an observation recorded in status, not a
  function of the spec. The record lists the consequences, is candid that ADR 0016's load-bearing
  objection is unmet by the motivating use cases, and says what would make it right to abandon
  this. The README's "it will never run a Dockerfile" is rewritten accordingly — `ImageComposition`
  never will, and that is unchanged.

  Alpha limits: `push` only (a Job in another pod cannot write to the loopback-only serving
  endpoint), no GC integration, no attestations.
- **An `image` layer verb, and `base.ref`.** An image can now be a layer source, and a base can be
  named as one conventional `repo:tag@sha256:…` string.

  The gap this closes is not "it cannot run a Dockerfile" — it is that **this tool ate tarballs and
  half the world ships images**. A CI pipeline's natural output is an image, and until now one could
  enter a composition only as `spec.base`: one per artifact, always underneath everything, always at
  `/`, with no `to`, `subpath`, `owner` or `mode`. "Put the contents of this image at `/plugins`"
  was not expressible, and the workaround — unpack it in CI and republish a tarball — discards the
  digest that made it trustworthy.

  **The image is flattened to exactly one layer**, with its whiteouts applied, so what lands is the
  filesystem a runtime would see — a constraint rather than an implementation detail, for the reason
  ADR 0024 gives. Everything downstream — `subpath`, rebasing, mode normalisation, symlink handling,
  traversal refusal, deterministic ordering — is the existing tar path unchanged, so an image and a
  tarball of the same content produce the same layer.

  The cost is blob sharing. `spec.base` reuses a base's layers verbatim so two artifacts on one base
  share blobs; flattening re-packs the bytes. Use `base` to build *on* an image and `image` to take
  files *from* one. Config is inherited only from the base, never from an `image` layer.

  `base.ref` settles a job ADR 0017 left open — the split `image` + `digest` pair is invisible to
  the two things that keep pins fresh, a Renovate regex and kustomize's `images` transformer, which
  both expect one string. Both spellings are supported, exactly one may be set, and they resolve and
  **hash** identically, so rewriting a spec from one to the other republishes nothing. The tag in a
  ref is decoration: what is pulled is always the digest.

  The scope line does not move. Nothing is executed; the input is a digest-pinned image, which is
  content-addressed more strictly than a `fetch` whose digest is merely declared. See
  [ADR 0024](docs/adr/0024-images-as-layer-sources.md).

  The README now also carries the recipe it was missing — CI → `crane digest --platform` → `ref` →
  spec-hash tag — and states plainly that `unpack: deb` resolves no dependencies.

- **`unpack: zip`**, plus `tar.xz`, `tar.zst`, `tar.bz2` and single-file `gz`. A great deal of
  software is published only as a `.zip` — Kafka Connect plugin bundles, JVM distributions,
  Terraform providers — and none of it could enter an artifact. The workaround was to repack the
  zip as a tarball and host that, which discards the upstream URL and digest and replaces them with
  "we repacked this once".

  `subpath`, rebasing, mode normalisation, symlink handling, deterministic ordering and traversal
  refusal are the same code the tar path uses, now extracted so there is exactly one implementation
  of where an entry is allowed to land. A zip and a tarball of the same content produce the same
  layer.

  **A zip records unix permissions only if whoever wrote it did.** An archive produced on Windows
  carries FAT attributes instead, so every file arrives non-executable and a binary needs
  `mode: {file: "0755"}` to be runnable. The output is still reproducible — the input is
  digest-pinned — but the same release zipped on Linux and on Windows gives two different
  artifacts, and nothing here can tell which you have. Guessing from content was rejected: it would
  make the output depend on something other than the spec.

  Symlinks are recovered properly, which is the part worth stating: a zip has no typeflag, so a
  symlink is an ordinary entry whose body is the link target. Reading entries as files would turn
  each one into a small text file containing a path, with a stable digest and a green build,
  failing later as a linker error in whatever mounts the artifact. Encrypted entries, compression
  methods beyond store and deflate, duplicated names and names that are not valid UTF-8 are refused
  rather than guessed at.

  `gz` unpacks a single compressed file, so `to` must name a file and `subpath` is invalid. The name
  comes from `to` alone — not from the URL, and not from the filename gzip stores in its header —
  because both are excluded from the input hash, and using either would let two mirrors of
  identical bytes produce different layers under one hash.

  The compressed-tar variants were nearly free: `unpack: deb` already needed xz, zstd and bzip2
  readers, since dpkg picks the compressor for `data.tar.*`. That table is now shared, so a spec can
  use xz on its own and not only inside a `.deb`. No new dependency. `tar.bz2` has no round-trip
  test because the standard library decodes bzip2 but cannot write it — the same two lines of stdlib
  the deb path has shipped since 0.4.0.

  **`AssemblyVersion` is unchanged, deliberately.** It covers output changing for *identical*
  inputs, and no existing spec can name any of these modes — the CRD's enum rejected them at
  admission. Bumping it would invalidate every recorded input hash in every cluster and force a
  full rebuild of byte-for-byte identical content.

  **Upgrading the chart is not enough.** Helm installs the CRDs under `crds/` on install and never
  touches them on upgrade, so a chart upgrade alone leaves the old enum in place and the API server
  rejects `unpack: zip` at apply time. Apply the CRD (`make install`, or
  `kubectl apply -f config/crd/bases`). Anyone validating manifests with kubeconform against a
  pinned `schemas/` URL needs to move that pin too, or valid manifests will fail their CI.

  See [ADR 0023](docs/adr/0023-more-archive-formats.md). RPM is still not included
  ([#9](https://github.com/lhns/kube-oci-composer/issues/9)); Alpine `.apk` still needs nothing.

- **An e2e test that answers ADR 0025's first spike question.** The alpha shipped without measuring
  whether `SOURCE_DATE_EPOCH=0` plus `rewrite-timestamp=true` actually gives byte-identical output
  across two runs of the same context, and that measurement is the difference between two readings
  of `status.inputHash`: whether it identifies the OUTPUT, or only the INPUTS.

  If rebuilds reproduce, the immutable-tag guard can never fire on an unchanged spec. If they do
  not, ADR 0025's concession stands -- losing status or the store means a rebuild can produce a
  digest that permanently conflicts with an already-published tag under the default
  `immutable: true`.

  **The measurement passed:** two independent builds of the same context, caches disabled, produce
  the same digest. Narrowly, though -- it shows the machinery does not inject nondeterminism, not
  that an arbitrary Dockerfile reproduces. A `RUN` that installs packages, resolves DNS or reads the
  clock can still differ, so the immutable-tag guard stays load-bearing.

  The test builds the same context under two names rather than deleting and recreating one object,
  because a deterministic Job name means a recreated object can *adopt* the first build's finished
  Job and read its digest back without building anything -- passing while proving nothing. The
  cache is disabled on both for the same reason: a cache hit would make the digests match by reuse
  rather than by reproducibility.

### Changed
- **BREAKING: `DockerBuild` is renamed `ImageBuild`.** Nothing in the implementation uses Docker --
  the build runs rootless BuildKit driven by `buildctl`, and what the kind consumes is a *Dockerfile*.
  [ADR 0028](docs/adr/0028-the-kind-is-called-imagebuild.md) records why this reverses
  [0025](docs/adr/0025-dockerfile-builds-as-a-second-kind.md)'s naming.

  **Upgrading requires manual action; existing objects are not migrated.** Kubernetes cannot rename
  a CRD's `spec.names`, so `imagebuilds.oci.lhns.de` is a different resource and nothing carries
  objects across:

  ```
  kubectl get dockerbuilds -A -o yaml > builds.yaml
  # rewrite `kind: DockerBuild` to `kind: ImageBuild`
  kubectl apply -f builds.yaml
  kubectl delete crd dockerbuilds.oci.lhns.de
  ```

  Helm never upgrades or deletes `crds/`, so a chart upgrade leaves the old CRD in place and adds
  the new one -- delete the old one by hand, and note that doing so deletes anything still under it.

  Status is lost with the objects, so every build runs once more. That is harmless today only
  because `push.immutable` is inert on this kind; it is why the rename lands before the tag policy
  is made real.

  The short name becomes `ibuild`. **The component keeps its name** -- the binary is still
  `oci-builder`, the chart still `kube-oci-builder` -- because neither ever contained "docker".
- **A `sourceRef` must now name a source in the object's OWN namespace.** Both controllers read Flux
  sources cluster-wide, so honouring `sourceRef.namespace` let anyone who could create an
  `ImageComposition` or `DockerBuild` pull another namespace's content into an image they control
  and can read — the one tenancy boundary a spec could cross on its own. A source elsewhere is now a
  terminal error naming the reason. The field is kept so an explicit same-namespace value is not a
  schema error. **Breaking** for anyone who genuinely relied on a shared source namespace: copy the
  source into the consuming namespace, or publish it to a registry and consume it as an image.
- **`sourceRef` layers hash the artifact's REVISION rather than its tarball digest.** Source
  controller re-packs artifacts on restart, so the digest moved while the content did not, and every
  composition consuming that source rebuilt for bytes that were identical. The digest is still what
  the fetch is verified against. One-time effect: input hashes for `sourceRef` layers change once,
  so those objects rebuild — and because composition is deterministic they reproduce the same digest,
  so no tag conflicts.

- The `DockerBuild` reconcile loop has tests: coverage of `internal/buildcontroller` goes from
  **23.6% to 82.9%**. The whole state machine was previously unexercised — only the pure Job
  rendering was covered — which is how the three defects above shipped. The new suite drives the
  loop over a fake client: job creation, adoption on repeat reconciles, the input-hash
  short-circuit, success recording the artifact, failure not stalling, suspend, a missing source
  being pending rather than terminal, and an unpinned `FROM` being refused before any Job exists.
- ADR 0025 corrected: it said `BuildRecord.inputHash` lets a controller that lost `status.artifact`
  re-verify rather than rebuild. The field is recorded but the read path is not implemented, and
  the record now says so.

- **`DockerBuild` has an e2e.** The first thing that runs the builder end to end, and it exists
  because two pieces could not be verified any other way: the built digest comes back through the
  pod's termination message, which needs a real kubelet to populate, and the `FROM` check reads a
  real context over HTTP. Both were previously tested only against fakes -- which is how a digest
  readback from a message nothing wrote got as far as it did.

  It also answers ADR 0025's second spike question, whether rootless BuildKit runs on the target
  nodes at all. The ADR lists "it does not" as grounds to abandon, so the failure is loud rather
  than skipped.

  Four cases: a build produces an image and the recorded digest resolves **in the registry** (not
  merely in status); an unchanged reconcile does not rebuild, with the input hash, the digest and
  the Job count all holding still; and an unpinned `FROM` is refused with **no Job created**.

  The fixtures are the interesting part. A build needs a registry, so one runs in the cluster over
  plain HTTP. The context must come from a Flux source, and the e2e cluster does not run Flux -- so
  a minimal `GitRepository` CRD stands in, deliberately without a status subresource so the harness
  can publish `status.artifact` itself, pointing at a tarball served from a ConfigMap. That tests
  this controller's reading of the contract rather than testing Flux.
- **The builder chart is now drift-guarded like the composer's.** `config/rbac-builder/role.yaml`
  was generated and read by nothing, so the chart's hand-written rules — the ones granting
  `jobs: create`, which is the ability to run arbitrary containers — could diverge from the
  kubebuilder markers unnoticed. Four guards mirror the composer's, reusing its helpers rather than
  copying them: RBAC matches the generated role, secrets are never `list`/`watch`, every rendered
  flag exists in the binary, and an unpinned builder image is refused. A fifth checks the shipped
  digests are not placeholders, which is the specific failure that got through.
- The `DockerBuild` reconcile loop has tests: coverage of `internal/buildcontroller` goes from
  **23.6% to 82.9%**. The whole state machine was previously unexercised — only the pure Job
  rendering was covered — which is how the three defects above shipped. The new suite drives the
  loop over a fake client: job creation, adoption on repeat reconciles, the input-hash
  short-circuit, success recording the artifact, failure not stalling, suspend, a missing source
  being pending rather than terminal, and an unpinned `FROM` being refused before any Job exists.
- ADR 0025 corrected: it said `BuildRecord.inputHash` lets a controller that lost `status.artifact`
  re-verify rather than rebuild. The field is recorded but the read path is not implemented, and
  the record now says so.

- **Build pods permit privilege escalation and add two capabilities.** Measured, not chosen: the
  first end-to-end run against a real cluster showed rootless BuildKit could not start under the
  previous posture, and [ADR 0027](docs/adr/0027-what-rootless-buildkit-actually-needs.md) records
  the six configurations tried.

  "Rootless" means no root on the *host*, not that no privilege is needed to start. Building an
  image means creating files owned by many UIDs, which needs a user namespace mapping a *range* of
  them -- and the kernel lets an unprivileged process map only one by itself, so the image ships
  setuid-root `newuidmap` to do that single write. `allowPrivilegeEscalation: false` made the kernel
  ignore the setuid bit, and `drop: ALL` emptied the bounding set so `CAP_SETUID` was unobtainable
  regardless. buildkitd never started and every build failed in seconds.

  Build containers now run with escalation permitted, all capabilities dropped, and exactly `SETUID`
  and `SETGID` added. Unchanged: uid 1000, `runAsNonRoot`, `privileged: false` (still not offered at
  any setting), no host namespaces, no devices, no host mounts. The controller's own posture is
  untouched.

  The cost, stated plainly: a setuid binary inside a build image can acquire those two capabilities
  within the container. That is not host root, and the blast radius ADR 0001 refused is not
  reinstated -- but it is a real loosening, and it is another reason the builder is a separate
  component with its own chart rather than a flag on the composer. Kubernetes user namespaces would
  have cost nothing and kept the old posture; all four variants failed to start on kind, so that
  remains the destination rather than the current state.

### Fixed
- **Anything in the cluster could push to the serving endpoint.** `--serving-bind-address` defaults
  to `:5000` -- every interface -- the chart exposes that as a Service, and the Ingress routes
  `/v2/`, a prefix that includes `PUT`. There is no authentication in that package. So any pod able
  to reach the Service could publish a manifest or repoint a mutable tag, and a test confirms an
  arbitrary pod's `PUT` returned **201 Created**.

  Writes are now refused unless the TCP peer is loopback, which means the controller itself. Reads
  stay anonymous, which is the point of the endpoint and unchanged.

  The uncomfortable part is how long it was written down without being true: `internal/serve`'s
  package doc has always claimed writes arrive only over loopback, and `Handler` said "the chart
  binds the write path to localhost". Neither was implemented, nothing tested it, and
  [ADR 0025](docs/adr/0025-dockerfile-builds-as-a-second-kind.md):87-90 built a design decision on
  the same false premise.
- **Long event messages were dropped by the API server.** The two controllers each had their own
  `event` helper and they were not the same: the builder truncated to the API server's limit, the
  composer did not, so an over-long message was rejected outright rather than shortened. The cases
  that produce one -- a build's stderr, a list of every unpinned `FROM` -- are exactly the failures
  worth seeing. One shared helper now, which is also how the difference was noticed.
- **An interrupted layer pull could never resume.** containerd asks for `Range: bytes=<offset>-`
  when continuing a download, which is valid per RFC 9110; the upstream registry handler parses the
  header with `fmt.Sscanf(h, "bytes=%d-%d")` and answered **416 BLOB_UNKNOWN**, so an interrupted
  pull failed permanently instead of continuing. The open-ended form is now closed to
  `bytes=<offset>-<size-1>` before the handler sees it -- wrapped rather than forked, since the size
  is only knowable from our blob store. Suffix ranges are deliberately left alone: upstream rejects
  those too, but containerd does not send them.
- **A standby replica could serve 404 for one tag forever.** Replay skipped a build when its
  *digest* was already present, but tag restores happen afterwards and only log on failure. One
  failed tag PUT therefore became permanent: the digest stayed present, the build was skipped on
  every later pass, and that tag 404'd on that replica for the life of the process while another
  replica served it -- which from a client is indistinguishable from a registry intermittently
  losing images. The skip now requires the digest **and** every tag.
- **The chart could template a replica that reports ready and serves nothing.** `replicaCount > 1`
  with `storage.shared=true` and `persistence.enabled=false` gave every pod its own `emptyDir` while
  asserting they were shared. Each replica then restored nothing, reported ready anyway (readiness
  is observed on attempt, deliberately), joined the Service and 404'd every pull routed to it.
  Refused at template time.

### Added
- **A golden-digest test for assembly.** `AssemblyVersion` exists so a controller that assembles
  differently cannot serve old-algorithm artifacts under an unchanged input hash -- but it is a
  constant a human must remember to bump, and the determinism test cannot help because it runs the
  algorithm twice in one process, where any change agrees with itself. The golden test pins the
  actual bytes, so a change to the tar writer, the config, the ordering or the toolchain's flate
  output fails loudly and prompts the decision.
- **`DockerBuild` history duplicated entries a rebuild reproduced.** It rotated `status.history`
  with its own copy of the logic, missing the composer's rule that a rebuild reproducing an earlier
  digest MOVES that entry to the front rather than adding a second one. Now that rebuilds are known
  to reproduce ([ADR 0027](docs/adr/0027-what-rootless-buildkit-actually-needs.md)) that was no
  longer theoretical: a reverted change, or any input-hash move that leaves the output identical,
  burned two retention slots on one artifact and evicted a genuinely distinct older build. Both
  kinds now share one rotation.
- **`flux reconcile` timed out instead of reporting a failure.** `status.lastHandledReconcileAt` and
  `status.observedGeneration` describe a reconcile pass, not its outcome, and Flux writes them that
  way -- the composer set both only on success. So a failing object never echoed the request the CLI
  was waiting for, and a stale `observedGeneration` reads to kstatus as "still working" rather than
  "failed". Both are worst exactly when someone is debugging. They are now set on every status
  write. `DockerBuild` never implemented the annotation at all despite declaring the field and
  [ADR 0009](docs/adr/0009-flux-conventions-without-dependency.md) committing to it; it does now.
- **The builder no longer requests `pods/log`.** Nothing read pod logs -- the digest comes back
  through the termination message and a failure's detail from container status -- so the grant was
  permission held for nothing.
- **A composition could publish a new tag holding the PREVIOUS revision's content**, permanently.
  A generator bumped a `GitRepository`'s `ref.tag` and rotated the composition's spec-hash publish
  tag in one apply; the composition reconciled instantly, the source had not cloned the new tag
  yet, and its `status.artifact` still described the old revision while reporting `Ready=True`.
  The controller believed it, the layer cache served the old tarball from disk without a network
  call, and the new tag was published pointing at old content. The immutable-tag guard could not
  catch it — a tag's *first* publish has nothing to conflict with — and could only refuse to
  correct it afterwards, which is how it was found.

  A Flux source's `status` is now only believed when it describes the source's current spec:
  `metadata.generation` must equal `status.observedGeneration`, and `Ready` must not be `False`.
  Otherwise the composition waits with `Reconciling`/`DependencyNotReady` instead of building.
  Flux source kinds are also watched now, so a source catching up rebuilds within seconds rather
  than at the next `spec.interval` — tolerating a cluster with no Flux installed, where the kinds
  simply are not watched. This does **not** cover a source tracking a branch or a semver range,
  which produces a new revision with no generation bump at all; see ADR 0026 for the
  `sourceRef.revision` follow-up that would.
- **`DockerBuild`: three things that were shipped wrong.** Found by analysing the merged alpha
  rather than the branch it came from.

  The chart's `buildkitImage` and `dockerfileFrontend` were pinned to **all-zero placeholder
  digests**. The guard checks for `@sha256:` — form, not substance — so the chart installed
  happily and every build then failed to pull. Both now carry real digests.

  **`spec.timeout` was never read.** A documented field with a 30m default that did nothing, so a
  hung build ran until something else killed it. It becomes the Job's `activeDeadlineSeconds`, so
  Kubernetes enforces it and marks the Job `DeadlineExceeded` — a controller-side timer would have
  had to survive a leader change to mean anything.

  **Build pods carried an API token.** `spec.serviceAccountName` defaulted to empty, so pods ran as
  the namespace's `default` account with its token mounted, while the chart created a purpose-built
  empty account that nothing could reference and `NOTES.txt` printed a line claiming builds used it.
  The account could never have worked: a ServiceAccount is namespaced and builds run in their own
  object's namespace. The guarantee was never "a special account", it was "no credentials in the
  build pod" — so the controller now sets `automountServiceAccountToken: false` on every build pod
  that does not name an account, which works in every namespace and needs no chart coordination.
  Naming an account is opting the token back in, for a build that genuinely needs an identity.

- **A suspended `DockerBuild` said nothing**, so it looked stalled. It now reports `Ready=False`
  with reason `Suspended`, matching `ImageComposition`.
- **No Events were emitted** despite the RBAC granting them. Build failures and invalid specs now
  raise Warnings — a build's detail lives in pod logs that vanish with the pod, so the Event is
  often the only durable trace.
- **`spec.resources` reached only the build container**, leaving the context fetch as the one
  unbounded container in the pod — the wrong one to leave unbounded, since it downloads somebody
  else's tarball.

- **`DockerBuild`: every build failed to find its own Dockerfile.** The two halves of the context
  contract disagreed, and each half was individually right.

  A source-controller artifact wraps the tree in a single top-level directory whose name is not
  predictable. The controller's pinned-`FROM` check strips that wrapper, so an unpinned base was
  correctly refused -- and then every build that PASSED the check died inside BuildKit with
  `failed to read dockerfile: open Dockerfile: no such file or directory`, because the init
  container extracted the archive verbatim and left the Dockerfile one directory below where
  `buildctl` looks.

  The init container now applies the same rule the controller does: strip one level only when the
  archive really is a single wrapper directory, so a tarball whose files sit at the root still
  builds rather than being silently emptied. The test runs the actual script against both shapes,
  because no assertion over the rendered pod spec could have caught this -- both halves looked
  correct in isolation, and only running them together showed the mismatch.

- **`DockerBuild`: a failing build retried in a hot loop and destroyed its own evidence.** Found by
  the first end-to-end run against a real cluster, which is the only place it could have been found:
  the reconcile loop was correct against a fake client, because a fake client has no watches.

  The failure path deleted the failed Job immediately, so that the next attempt would not adopt it.
  But deleting an owned Job wakes this controller through its own Job watch, and that reconcile
  finds no Job and starts another — so the `RequeueAfter` backoff never applied and the build
  retried every few seconds indefinitely. Each retry also deleted the previous pod, and the pod is
  the only place the reason a build failed is written down, so no one could ever read why.

  The failed Job is now kept until its backoff has actually elapsed and deleted only when the next
  attempt is due, which makes the delete the trigger for the retry rather than a race against it.
  A failure is counted once however many times it is observed.

  Relatedly, a failed build reported only `BackoffLimitExceeded` — the mechanism, not the cause.
  Status now carries the build container's exit code, termination reason and message, plus the
  `kubectl logs` line that shows the rest.
- **The e2e harness could not report its own failures.** `make e2e-test` ran `go test -timeout 15m`
  while the in-test deadline was also 15 minutes, so the test binary panicked on the global timeout
  at the same instant and the diagnostic dump never ran. The in-test deadline is now strictly
  shorter than the binary's, so the harness always outlives the assertion it is reporting on.
- **`DockerBuild`: three things that were shipped wrong.** Found by analysing the merged alpha
  rather than the branch it came from.

  The chart's `buildkitImage` and `dockerfileFrontend` were pinned to **all-zero placeholder
  digests**. The guard checks for `@sha256:` — form, not substance — so the chart installed
  happily and every build then failed to pull. Both now carry real digests.

  **`spec.timeout` was never read.** A documented field with a 30m default that did nothing, so a
  hung build ran until something else killed it. It becomes the Job's `activeDeadlineSeconds`, so
  Kubernetes enforces it and marks the Job `DeadlineExceeded` — a controller-side timer would have
  had to survive a leader change to mean anything.

  **Build pods carried an API token.** `spec.serviceAccountName` defaulted to empty, so pods ran as
  the namespace's `default` account with its token mounted, while the chart created a purpose-built
  empty account that nothing could reference and `NOTES.txt` printed a line claiming builds used it.
  The account could never have worked: a ServiceAccount is namespaced and builds run in their own
  object's namespace. The guarantee was never "a special account", it was "no credentials in the
  build pod" — so the controller now sets `automountServiceAccountToken: false` on every build pod
  that does not name an account, which works in every namespace and needs no chart coordination.
  Naming an account is opting the token back in, for a build that genuinely needs an identity.

- **A suspended `DockerBuild` said nothing**, so it looked stalled. It now reports `Ready=False`
  with reason `Suspended`, matching `ImageComposition`.
- **No Events were emitted** despite the RBAC granting them. Build failures and invalid specs now
  raise Warnings — a build's detail lives in pod logs that vanish with the pod, so the Event is
  often the only durable trace.
- **`spec.resources` reached only the build container**, leaving the context fetch as the one
  unbounded container in the pod — the wrong one to leave unbounded, since it downloads somebody
  else's tarball.

- **An archive entry named exactly `..` escaped its target directory.** The traversal guard tested
  for it in one condition and then only raised an error on a `../` prefix, which `..` does not have,
  so the entry survived and landed one level above where the layer was confined. The impact was a
  stray directory entry rather than overwritten content, and no legitimate archive contains such an
  entry — but it was a hole in the check whose entire job is to be the escape guard, and there was
  no test covering traversal at all. There is now, for every format.
- **Entries sharing a name were resolved by an unstable sort.** Two entries with the same name
  compare equal, so which one survived deduplication was decided by the sort's internal
  partitioning rather than by the archive. The result was deterministic for a given Go toolchain and
  undefined in principle, meaning a Go upgrade could silently move digests that immutable tags then
  refuse to republish. The sort is now stable, so archive order is the tiebreak. This is not an
  `AssemblyVersion` change: it only affects archives whose output was never well defined, and a
  version bump cannot repair a hash that had no single correct value.
- **An unpack mode the controller does not implement now stalls instead of retrying forever.** It
  was an ordinary error, so the object sat `Ready=False` and requeued with exponential backoff
  indefinitely, never setting `Stalled` and never saying why. It is reachable in practice precisely
  because Helm does not upgrade CRDs: a schema newer than its controller is an ordinary situation.

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
- Pod-reference protection in the garbage collector: implemented, measured to protect nothing, and
  removed. See [ADR 0011](docs/adr/0011-content-tags-expire.md).

[Keep a Changelog]: https://keepachangelog.com/en/1.1.0/
