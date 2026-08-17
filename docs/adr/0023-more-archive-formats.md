# 0023. More archive formats: `unpack: zip` and the compressed-tar family

## Status

Accepted. Extends [0022](0022-distro-packages-as-layer-sources.md), which named `collectEntries`
as the seam new formats slot into and predicted this record.

## Context

Layer content could arrive as a tar, a gzipped tar, a `.deb`, a ConfigMap, a Flux source or an
image. That covers the Linux release-tarball world well and the rest of the world not at all.

A great deal of software is published **only** as a `.zip`: Kafka Connect plugin bundles, JVM
distributions, Terraform providers, anything whose release process was written with Windows in
mind. None of it could enter an artifact. The workaround is to download the zip, repack it as a
tarball, and host that somewhere — which throws away the upstream URL and the upstream digest, the
two things [0002](0002-content-addressed-inputs.md) exists to preserve, and replaces them with "we
repacked this once".

The compressed-tar variants were a smaller gap of the same shape. `.tar.xz` is the normal
distribution form for a lot of source and toolchain releases, `.tar.zst` is increasingly common,
and both readers were **already vendored and already reachable** — `unpack: deb` needs them,
because dpkg chooses the compressor for `data.tar.*`. So a spec could use xz inside a `.deb` and
not on its own, which is arbitrary rather than principled.

## Decision

**Add `unpack: zip`, `tar.xz`, `tar.zst`, `tar.bz2` and `gz`.**

The codec table moved out of the deb reader into `compress.go` and is now shared. A codec is not a
property of the container that happened to need it first, and `tar.xz` is one table entry rather
than a decision.

`zip` is a genuinely new reader, because a zip is not a tar with a wrapper the way a `.deb` is.
Four differences are handled in `zip.go` and each one is somewhere a plausible implementation is
quietly wrong:

- **There is no typeflag.** A symlink is an ordinary entry whose *body* is the link target, marked
  only by a mode bit. Reading entries as files therefore turns every symlink into a small text
  file containing a path — silently, with a stable digest and a green build, surfacing much later
  as a linker error in whatever mounts the artifact. This is the same property `unpack: deb` was
  added to preserve, so it gets the same treatment and a test of its own.
- **Entry order is undefined and duplicate names are legal.** Order does not matter, because
  entries are sorted by name before the layer is written. Duplicates are **refused**: picking one
  would make the layer depend on which, and overlaying is what the ordered `layers` list is for.
- **Separators.** The format specifies `/`, but zips written on Windows do arrive with `\`. These
  are normalised *before* the traversal check, not after — checking first would let
  `..\..\etc\passwd` through as one very strange filename.
- **Permissions are optional.** See *Consequences*.

Entries that are encrypted, or that use a compression method beyond store and deflate, or whose
names are not valid UTF-8, are refused rather than guessed at. Each has more than one plausible
reading, and Go's `archive/zip` handles none of them: it does not check the encryption flag, so
reading on would produce a layer full of ciphertext.

Beyond that, nothing is new. `subpath`, rebasing, mode normalisation, symlink handling,
deterministic ordering and traversal refusal are the same code the tar path uses — extracted into
`extract.go` so there is exactly one implementation of *where an entry is allowed to land*. Two
copies of that check would mean the next hardening reaching one of them.

`gz` unpacks a single compressed file rather than an archive, so `to` must name a file and
`subpath` is invalid. The name in the image comes from `to` and nothing else: gzip records an
original filename in its header and the URL usually ends in one, and using either would make the
output depend on a field `InputHash` deliberately excludes. Two mirrors serving identical bytes
under different filenames would then produce different layers under one input hash, and the
reconciler would keep serving whichever was built first.

`AssemblyVersion` is **not** bumped. It covers output changing for *identical inputs*, and no
existing spec can name any of these modes — the CRD's enum rejected them at admission. Bumping
would invalidate every recorded input hash in every cluster and force a full rebuild of content
that is byte-for-byte identical.

This does not move the line [0016](0016-the-scope-line-is-determinism.md) draws. Nothing is
executed, nothing is installed, and every input is still a URL with a declared digest. `unpack:
zip` means "this archive has a wrapper I know how to remove", in exactly the sense that
`unpack: tar.gz` means "this archive is compressed".

## Consequences

**A zip does not portably record unix permissions, and this is the one place the format is worse
than a tarball.** `archive/zip` reads a mode only when the writer recorded a unix creator version;
an archive produced on Windows carries FAT attributes instead, which yield `0666` and therefore no
executable bit at all. Every file lands `0644` and a binary is not runnable.

The output is still perfectly reproducible — the input is digest-pinned, so the same bytes always
give the same layer — but it is *surprising*, and the same logical release zipped on Linux and on
Windows produces two different artifacts. The answer is `mode: {file: "0755"}` on the layer, which
already existed for other reasons, with the limitation that it applies uniformly to every file in
that layer and so cannot mark one binary executable and leave the rest alone. Splitting such a
release across two layers with different modes is the way out.

Deliberately not done: guessing. Sniffing content for an ELF header and inferring the exec bit
would make the output depend on something other than the declared spec, which is the property
everything here rests on.

**Extraction still holds a whole archive in memory**, and these formats make that easier to reach
rather than changing it. Entries are read into a slice before anything is written, so peak memory
tracks the uncompressed size of the largest layer; `xz` and `zstd` reach much higher ratios than
deflate. This is pre-existing — it is equally reachable through `tar.gz`, and `unpack: deb` already
exposed both of those codecs — and it is bounded by the fact that a spec author must pin the digest
of the thing that does it, so it is self-inflicted rather than an injection. It is nonetheless a
shared controller, so one namespace's spec can exhaust reconciliation for a whole cluster. A
uniform limit across all modes, and streaming rather than buffering entries, is the real fix and is
not in this change; a per-format limit was explicitly rejected, because advertising a bound on zip
alone implies the others are safe.

**Version skew is now visible.** An unpack mode the running binary does not implement is a typed
error mapped to `Stalled`, rather than an ordinary error retried with backoff forever. This matters
because the chart ships CRDs under `crds/`, which Helm installs and never upgrades, so a schema
newer than its controller is an ordinary situation rather than an exotic one.

## Alternatives rejected

**Tell people to repack zips as tarballs.** Works today, and it is what everyone does. Rejected
for the reason [0022](0022-distro-packages-as-layer-sources.md) rejected the same answer for
`.deb`: it moves a step into a place that is not written down, and the provenance of the result is
whoever ran `unzip` that day.

**A general "run a command to produce a layer" escape hatch.** Would cover zip, RPM and everything
else at once. Rejected on [0016](0016-the-scope-line-is-determinism.md): executing arbitrary code
during reconciliation is exactly what stops the output being a function of the spec.

**Add every format at once, RPM included.** RPM still needs a lead/signature/header parser and a
cpio reader — several times this much code — and remains declined until something needs it
([#9](https://github.com/lhns/kube-oci-composer/issues/9)). Alpine's `.apk` still needs nothing: it
is a gzipped tar and works through `unpack: tar.gz` with a `subpath`.

**Single-file `xz`, `zst` and `bz2` alongside `gz`.** Each is two lines given the shared table. Left
out because a lone `.gz` is a thing people actually ship and the others essentially are not, and
[0022](0022-distro-packages-as-layer-sources.md) is right that an extraction path nobody exercises
is how a format ends up subtly broken for its first user. Cheap to add when one appears.

**Transcode non-UTF-8 entry names.** Would accept a few more archives at the cost of a charset
table and a second way for two builds of this controller to disagree about what a layer contains.
Refusing is louder and cannot be wrong.
