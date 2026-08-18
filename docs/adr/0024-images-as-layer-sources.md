# 0024. An image can be a layer source, flattened

## Status

Accepted. Extends [0016](0016-the-scope-line-is-determinism.md) by adding a source, not by moving
the scope line, and settles the `base.image`/`base.digest` merge that
[0017](0017-updating-the-consumed-digest.md) left open.

## Context

The recurring need this project was built for is, in [0016](0016-the-scope-line-is-determinism.md)'s
words, *"take a released artifact and get it into an image"*. A released artifact is usually a
tarball, and `fetch` handles it. Increasingly often it is **an image** — because the thing that
released it was a CI pipeline, and a pipeline's natural output is an image pushed to a registry.

Until now such a release could enter a composition exactly one way: as `spec.base`. That is the
wrong shape for it more often than not.

- There is **one** base per artifact, and it sits underneath everything. Two CI-built components
  in one artifact cannot both be the base.
- A base is a *foundation*, not *content at a path*. "Put the contents of this image at
  `/plugins`" was not expressible at all.
- The base's target is implicitly `/`. There is no `to`, no `subpath`, no `owner`, no `mode`.

So the honest description of the gap was not "it cannot run a Dockerfile". It was **"this tool
eats tarballs, and half the world ships images"** — and the workaround was to unpack the image by
hand in CI and republish it as a tarball, which discards the digest that made it trustworthy and
adds a step nobody wrote down. That is the same failure [0022](0022-distro-packages-as-layer-sources.md)
described for `.deb`: *"the cheapest way around it is to extract the binary once by hand… and then
the provenance of a binary running in production is 'someone extracted it once'."*

Separately, [0017](0017-updating-the-consumed-digest.md) recorded an unfinished job: *"Merging
`base.image` and `base.digest` into one conventional `ref: repo:tag@sha256:…` remains worth doing
and is not done."* The split form is invisible to the two tools that keep pins fresh — a Renovate
`customManager` regex and kustomize's `images` transformer both expect one string.

## Decision

**Add an `image` layer verb, and a combined `ref` spelling for the base.**

```yaml
base:
  ref: quay.io/strimzi/kafka:0.43.0@sha256:…      # replaces image + digest

layers:
  - name: app
    image:
      ref: ghcr.io/me/app:v1.2.3@sha256:…
      subpath: usr/local/bin
    to: /opt/tools
    mode: {file: "0755"}
```

**The image is flattened to exactly one layer.** Its layers are walked in order and its whiteouts
applied, producing the filesystem a runtime would actually see; that stream is an ordinary tar, so
`subpath`, rebasing, mode normalisation, symlink handling, traversal refusal and deterministic
ordering are all the existing tar path's, unchanged.

Flattening is a constraint, not an implementation detail. [0016](0016-the-scope-line-is-determinism.md)
hoisted the base out of the layer list because *"an image entry contributes many layers where every
other entry contributes exactly one"* — one of the three exceptions that proved the abstraction was
wrong. Splicing a source image's layers through this verb would put that exception straight back.
With flattening, *"each entry produces exactly one layer"* stays true.

**This does not move the scope line.** Nothing is executed and nothing is installed. The input is a
digest-pinned image, which is content-addressed in exactly the sense [0002](0002-content-addressed-inputs.md)
requires — more strictly than a `fetch`, in fact, whose digest is *declared* by the spec author
while an image's is verified by the registry protocol. The output remains a pure function of the
resolved inputs, and the acceptance test at [0016](0016-the-scope-line-is-determinism.md):60-61 —
*"anything proposed for this API has to be a pure function of its inputs"* — is satisfied.

**The tag in a ref is decoration.** What is pulled is always the digest. A tag is recorded so a
human can see which release a digest corresponds to, and is ignored when pulling, because resolving
a tag at reconcile time is precisely what would stop the output being a function of the spec. The
two base spellings resolve identically and therefore hash identically — rewriting a spec from one
to the other must not republish anything.

## Consequences

**Layer sharing with the source image is given up.** `spec.base` reuses a base's layers verbatim so
that two artifacts on one base share blobs and the registry re-uploads nothing
([0015](0015-base-images-are-platform-pinned.md)). Flattening re-packs the bytes, so an `image`
layer costs a copy: the same content stored twice under different digests. That is the price of
choosing where the content lands, and it is why this verb does **not** replace `spec.base` for
building on top of something. Use `base` to build *on* an image; use `image` to take files *from*
one.

**Config is not inherited.** An `image` layer contributes files and nothing else — no entrypoint,
no env, no exposed ports. `config.inherit` reads the base and only the base. This is deliberate:
inheriting config from an arbitrary position in an ordered list would need rules about which entry
wins, and the base already exists for the case where config matters.

**Whiteouts inside the source are resolved, not forwarded.** A file deleted by a later layer of the
source image is absent from the contributed layer, rather than appearing as a `.wh.` marker that
would then delete a same-named file from *this* artifact's base. The two whiteout namespaces stay
separate, which is what makes the verb composable with `remove`.

**A multi-architecture index is refused**, exactly as it is for a base without `spec.platforms`
([0015](0015-base-images-are-platform-pinned.md)): selecting a child would mean the controller
choosing a platform.

**`base.image` and `base.digest` are kept.** They are not deprecated and not scheduled for removal.
Every existing spec keeps working, and CEL enforces that exactly one spelling is used so no object
can name its base twice.

## Alternatives rejected

**Splice the source image's layers instead of flattening.** Cheaper — it would preserve blob
sharing and skip the re-pack. Rejected for the reason in *Decision*, and because it cannot honour
`to`, `subpath`, `owner` or `mode`: those need the content unpacked, which is the whole point of
the verb. An entry that silently ignored four of its own options would be worse than not having it.

**Make it an `unpack: image` mode on `fetch`.** Superficially tidy, since `unpack` already selects
how bytes become content. Rejected because a registry pull is not an HTTP fetch — it is a manifest
resolution, an auth handshake and N blob fetches — and `fetch.url` + `fetch.digest` describe one
URL returning one body. Overloading them would make `digest` mean something different depending on
a sibling field.

**Use a Flux `OCIRepository` and `sourceRef`.** Already possible, and it works when CI publishes an
OCI *artifact*. It does not work for an ordinary image: `sourceRef` assumes source-controller's
gzipped tarball, which an image is not. Recommending it would be telling people to install another
controller and change what their CI publishes, to avoid a verb that is forty lines.

**Deprecate `base.image` and `base.digest` in favour of `ref`.** Rejected as churn. The split form
is not wrong, it is just illegible to Renovate; both spellings cost one accessor to support.

**Do nothing and document the workaround.** The workaround is "unpack the image in CI and publish a
tarball", which is exactly the provenance loss described in the Context. It is the status quo, and
it is what this record exists to end.
