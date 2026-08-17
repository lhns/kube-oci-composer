# Changelog

Notable changes. Format loosely follows [Keep a Changelog]; this project is pre-1.0 and the API
may change between minor versions.

## [Unreleased]

### Added
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

### Fixed
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
