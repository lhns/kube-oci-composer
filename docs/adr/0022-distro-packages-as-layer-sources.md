# 0022. Distro packages are a layer source: `unpack: deb`

## Status

Accepted. Extends [0001](0001-compose-dont-build.md) and [0016](0016-the-scope-line-is-determinism.md)
by adding an archive format, not by moving the scope line.

## Context

A layer's content had to arrive as a tarball, a ConfigMap, a Flux source or an image. That covers
application code and release archives well, and covers native libraries badly.

For a native library, the distribution's package is very often the only build that exists.
Upstream publishes source; a `.so` built for a particular libc, compiler and soname comes from
Debian, Ubuntu, Alpine or Fedora. Anyone wanting to add one to an image they do not control had
to go around this controller entirely (see *Alternatives rejected*), and the cheapest way around
it is to extract the binary once by hand and commit it. People do that because everything else is
disproportionate for one file, and then the provenance of a binary running in production is
"someone extracted it once".

A `.deb` is already an archive. It is an `ar` container holding `debian-binary`, `control.tar.*`
and `data.tar.*`, and the payload is an ordinary tar. Everything downstream of untarring —
subpath selection, rebasing, mode normalisation, symlink handling, deterministic ordering —
already exists and is unchanged by this.

## Decision

**Add `unpack: deb`, which extracts the package payload and nothing else.**

The `ar` container is parsed inline. It is a magic string and a run of fixed 60-byte headers;
a dependency would be more code to audit than the parser. GNU long names are refused rather than
guessed at, because `dpkg` does not emit them and a mis-read member name is a silently wrong
layer. `xz` is a new dependency (`github.com/ulikunitz/xz`) because it is what Debian mostly
uses and the standard library has no reader; gz, bz2 and zst are handled from what is already
present.

Only `data.tar.*` is read. `control.tar.*` is a tar too, and taking it would put a package
manager's metadata into the artifact.

**This installs nothing.** No dependency is resolved, no maintainer script runs, no package
database is touched. `unpack: deb` means "this archive has a wrapper I know how to remove", in
exactly the sense that `unpack: tar.gz` means "this archive is compressed". A package whose
files only work after `postinst` has run will not work, and that is the correct outcome: making
it work would mean executing vendor code during reconciliation, which is the line
[0016](0016-the-scope-line-is-determinism.md) draws.

Determinism is unaffected. The input is still a URL with a declared digest, and the same `.deb`
produces the same layer.

## Consequences

The caller now carries a compatibility obligation that a tarball did not impose. A `.so` links
against the C library and the sonames of whatever the packaging distribution shipped, so a
package must be chosen to match the image it is mounted into — Debian 12's build into a Debian 12
image. Nothing here can check that: the artifact is built without reference to its consumer, and
the consumer mounts it without inspecting it.

The failure mode is at least loud and local. A mismatched library fails to load in the process
that needs it, with a linker error naming the missing symbol or soname. It does not corrupt
anything and it does not silently half-work.

Two consequences follow for anyone using this:

- **Pin the package the way you pin everything else.** The URL includes the distribution's
  version-and-revision, and the digest is declared. A `.deb` URL in a distribution's pool is
  *not* stable — Debian removes superseded revisions from `pool/` once they leave the archive —
  so the URL will eventually 404. That surfaces as a failed fetch, not as wrong content.
- **Move the pin when the consuming image moves.** A base image jumping to a new distribution
  release is the moment to re-pin, and there is no automation that will notice for you.

Other package formats are deliberately not included. Alpine's `.apk` is already a gzipped tar and
works today through `unpack: tar.gz` with a `subpath`. RPM would need a lead/signature/header
parser and a cpio reader — several times this parser — and adding an extraction path nobody
exercises is how a format ends up subtly broken for the first person who tries it. It is a
reasonable addition when something needs it, and `collectEntries` is the seam it slots into.

## Alternatives rejected

**Build a derived image.** The obvious answer, and the reason [0001](0001-compose-dont-build.md)
exists: it puts a container build in the release path of a workload whose only change is one
file, and couples every upstream release of the base to a rebuild somebody has to remember.

**An initContainer that installs the package at pod start.** Always matches the running base
image, which is a genuine advantage this decision gives up. Rejected because it makes every pod
start depend on a package mirror being reachable, and because the result stops being a function
of anything recorded — two pods of the same Deployment can get different bytes.

**Extract the payload elsewhere and feed it in as a tarball.** Correct, and it works today. It
just moves the `ar` parsing into a step that is not written down, which is the situation this
record is about.

**A general "run a command to produce a layer" escape hatch.** Would cover `.deb`, RPM and
anything else in one field. Rejected on [0016](0016-the-scope-line-is-determinism.md): executing
arbitrary code during reconciliation is precisely what makes output stop being a function of the
spec.
