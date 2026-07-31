# 0013. Persist manifests so published builds survive a restart

## Status

Proposed. Not yet implemented. The defect it describes is measured and present today.

## Context

Manifests live in `pkg/registry`'s in-memory map (0012), and the startup reconcile republishes
only the **current** build of each object. Everything older is gone.

Measured with a persisted blob store and a fresh registry:

| reference | after restart |
|---|---|
| current moving tag `:main` | resolves |
| current digest `@sha256:…` | resolves |
| blob re-upload | none — blobs reused |
| **older build's content tag** | **404** |
| **older build's digest reference** | **404** |

The second failure is the serious one, and it was not what the earlier reasoning anticipated. It
is not only the hand-pinning fallback from 0010 that breaks — **an older build's digest reference
404s too**, and that is the primary path. `image-automation` writes a digest into git; if a pod is
still on the previous digest when the controller restarts, that digest is unresolvable and the pod
cannot start on a fresh node. Spegel covers nodes that already pulled it; a newly scheduled one is
not covered.

Garbage collection is unrelated to this: retention keeps the *blobs*, and the manifest that names
them is lost anyway.

## Decision

**Persist manifest bytes alongside blobs, and re-seed them at startup.**

- At publish time, write the manifest into the store under a `manifests/` namespace, keyed by its
  digest, together with the tags pointing at it.
- At startup, read back the manifests for retained builds and `PUT` them to the loopback endpoint.
  The blobs are already present, so this is a small write per build and nothing else.

No new registry implementation is required — this was the fear in 0012 and it was misplaced. The
endpoint already accepts manifest `PUT`s; the only thing missing is remembering what to replay.

## Consequences

Content tags and digest references survive a restart for as long as retention keeps them (0011),
which is what 0010 assumed all along.

It also produces a **manifest-to-blob mapping that outlives `status.history`**. That is precisely
what was missing when Pod-reference protection was tried and removed in 0011, so that rail becomes
implementable afterwards and should be reconsidered then.

Storage grows by the size of the retained manifests, which is kilobytes.

The store gains a third namespace alongside `inputs/` and `blobs/`, and garbage collection has to
mark and sweep it too — a manifest whose build has aged out is reclaimable exactly when its blobs
are.

## Alternatives rejected

**Write our own registry so manifests are first-class.** The thorough fix, reconsidered here and
still rejected: replaying manifests achieves the same durability without owning distribution-spec
compliance. Revisit if replay proves fragile.

**Republish every retained build by re-assembling it.** Impossible in general — the spec may have
changed, so the inputs for an older build are no longer described anywhere. Replaying stored
manifests does not need them.

**Accept the limitation and document it.** Considered, and rejected once the measurement showed
digest references were affected too. A documented caveat is defensible for a fallback path; it is
not defensible for the primary one, where the failure surfaces as `ImagePullBackOff` on an
unrelated pod after an unrelated restart.

**Rely on Spegel.** It mirrors what nodes already hold, so it helps exactly where the problem is
absent and not at all where a pod is newly scheduled.
