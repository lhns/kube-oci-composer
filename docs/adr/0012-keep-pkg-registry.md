# 0012. Keep `pkg/registry`; own only the blob handler

## Status

Accepted.

## Context

Serving mode (0006) needs an OCI distribution endpoint. `go-containerregistry`'s `pkg/registry`
provides one, and its README says it is "designed to be used anywhere a low dependency container
registry is needed, with an initial focus on tests" — production use invited, not claimed.

Reading the implementation turns up hard constraints:

- **Blobs are pluggable**: `WithBlobHandler`, plus optional `Stat`, `Put` and `Delete` interfaces,
  and a `RedirectError` that is honoured on blob `HEAD` and `GET`.
- **Manifests are not.** They live in an unexported `map[string]map[string]manifest`, initialised
  unconditionally in `New()`, with no option, interface or seed hook. The package forbids new
  dependencies by design, so a persistence hook will never be accepted upstream.
- **Upload state is process-local**, in an in-memory map, and cross-repo mount is unimplemented.
- **`NewDiskBlobHandler.Stat` re-reads and re-hashes the entire blob on every call**, and `Stat`
  runs on every `HEAD` and again before every `GET`. A 200 MB artifact is read twice per pull to
  re-derive a digest that was verified when it was written.

The question is whether to write our own read-only distribution handler over a pluggable store —
roughly 250 lines of read path — or keep the upstream one.

## Decision

**Keep `pkg/registry`. Replace only the blob handler.**

The in-memory manifest map looked like a blocker for durable storage. It is not, because the
manifest map is repopulated by *pushing to it*, which the startup reconcile already does. This was
measured: with a persisted blob store and a fresh registry, the current build's moving tag and
digest reference both resolve after a restart, and **no blobs are re-uploaded** — `remote.Write`
HEADs each blob, finds it present, and skips it. The republish is one small manifest.

What that leaves expensive is the *re-fetch* of inputs, and that is what the cache addresses
(0014), not a different registry.

**Our blob handler** implements `Get`, `Stat`, `Put` and `Delete` over the `Store` interface, for
three reasons in order of weight:

1. Garbage collection needs deletion and enumeration (0011).
2. Object storage for served blobs becomes possible (0014).
3. It fixes the re-hash-per-`Stat` cost above.

Presigned redirects are supported and off by default. `Stat` answers directly and only `Get`
redirects: `HEAD` is a client asking whether a blob exists, and forcing a round trip to object
storage to answer something the controller already knows is backwards. A presigning failure falls
back to streaming — a reason to be slow, not to fail a pull — and a miss still 404s rather than
handing out a URL that will 404 later.

The `repo` argument is ignored, as upstream ignores it. The same digest is the same bytes, so
keying by repository would store a shared layer once per composition and defeat exactly the
sharing content addressing provides.

## Consequences

**Manifests remain process-local**, which has one consequence worth its own record: older builds
do not survive a restart (0013).

**Replicas cannot share the served store**, because upload state and manifests are both
process-local. Handled by making the endpoint leader-election scoped, with readiness keeping
standbys out of the Service — active/standby rather than scale-out (0006, 0007).

The upstream "aimed at tests" caveat is inherited. The exposure is bounded: read-only to the
network, reproducible content, writes only over loopback, and Spegel absorbs unavailability for
anything already pulled.

## Alternatives rejected

**Write our own read-only distribution handler.** Attractive — manifests would persist, S3 would
work fully, GC would be direct store operations, and there would be no loopback push. Rejected
because the measurement above showed the manifest map is not the bottleneck the cache is, and
because it means owning distribution-spec compliance for a read path that upstream already
implements including the Referrers API. Worth revisiting if 0013's approach proves insufficient.

**Fork `pkg/registry` to add a manifest handler.** All the maintenance of a fork, and upstream
will not take the change back because of the no-dependencies rule.

**Keep `NewDiskBlobHandler`.** Cannot enumerate for GC, cannot use object storage, and re-hashes
every blob on every pull.
