# Changelog

Notable changes. Format loosely follows [Keep a Changelog]; this project is pre-1.0 and the API
may change between minor versions.

## [Unreleased]

### Changed
- **`publish.tag` and `push.tag` are now `tags`, a list.** Optional, with no default: omit it and
  the artifact is published by digest alone. One build can carry several — a spec-hash tag
  alongside a readable pointer, or the same hash under more than one algorithm.
- **`immutable` now defaults to `true`, on both `publish` and `push`.** It previously existed only
  on `push` and defaulted to false. The controller refuses to move a tag to different content and
  fails the build instead. Republishing *identical* content remains a no-op, so a steady reconcile
  loop never trips it; set `immutable: false` for a deliberately moving pointer such as `main`.
- **The auto-generated `<tag>-<digest[:12]>` content tag is gone**, along with
  `status.artifact.contentTag` (now `status.artifact.tags`) and `BuildRecord.contentTag` (now
  `tags`). It existed only because the tag was a moving pointer and nothing else offered an
  immutable handle; a spec-hash tag is one, and the digest always was.

  Migration: replace `tag: x` with `tags: [x]`, and add `immutable: false` if that tag is meant to
  move. Anything pinning a content tag should pin the digest or a spec-hash tag instead.

### Added
- **Spec-hash tags**, resolving [ADR 0017](docs/adr/0017-updating-the-consumed-digest.md) — how a
  workload's reference gets updated. The consumer hashes the build-determining part of the spec
  and writes the result into both `publish.tags` and its own image reference, so the two stay in
  step with no image automation, no git write-back and no status reading. Worked example in
  `docs/examples/spec-hash-tag/`.
- `ImageComposition` API (`oci.lhns.de/v1alpha1`) with `url` layer sources, deterministic
  assembly, and publication by digest plus any requested tags.
- Built-in read-only OCI serving endpoint, so no registry is required.
- Flux conventions: kstatus conditions, `suspend`, `interval`, `observedGeneration`,
  `status.artifact`, and the `reconcile.fluxcd.io/requestedAt` annotation.
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
- Architecture decision records in `docs/adr/`.

### Fixed
- Cache returned a path to a file it had already deleted when the remote tier was unavailable.
- Unclosed file handle leaked on every cache miss.
- Disk traversal guard was ineffective on Windows because `filepath.IsAbs` is false for
  `/etc/passwd` there.
- `name.Insecure` was applied to every reference, which would have silently downgraded pushes to
  an external registry to plaintext HTTP.
- `status.artifact.ref` was built from the loopback address rather than the serving host.

### Removed
- Pod-reference protection in the garbage collector: implemented, measured to protect nothing, and
  removed. See [ADR 0011](docs/adr/0011-content-tags-expire.md).

[Keep a Changelog]: https://keepachangelog.com/en/1.1.0/
