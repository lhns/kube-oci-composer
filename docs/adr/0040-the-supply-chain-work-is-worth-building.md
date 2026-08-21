# 40. The supply-chain work is worth building, and here is what it costs

Date: 2026-08-21

## Status

Accepted. **Closes [0020](0020-is-the-supply-chain-work-worth-building.md)**, and amends
[0008](0008-supply-chain.md), whose *how* stands except on one point.

## Context

0008 answered *how* — attestations as referrers, key-based cosign signing, never keyless. 0020
asked *whether*, and stayed open. **Two of the three things holding it open were resolved by work
done for other reasons.**

0020 worried that "the mode most users run is the mode least able to hold attestations": the
embedded registry's referrers implementation linear-scanned every manifest in a repository per
request. And it offered option D, "push-mode only", as a narrowing. [0035](0035-a-registry-is-the-only-publication-path.md)
deleted the embedded endpoint, so a real registry is now the only publication path — D is not a
narrowing of A, it *is* A, and the objection has no referent.

What is left of 0020 is its central point, which is not resolved and is not resolvable here:
producing signatures changes nothing on its own.

## Decision

**Build it. Option A, both halves, both kinds, everything off by default.**

- **The composer derives its SBOM and provenance** from inputs that are already digest-pinned, so
  the statement is exact rather than inferred. 0008's rejection of scanning stands: the inputs are
  known, and scanning would replace a precise statement with a guess.
- **The builder uses BuildKit's own `attest:sbom` and `attest:provenance`.** A build that runs
  `apt-get install` cannot state what it installed; only the build can see that. 0008 already names
  this asymmetry and this record adds a second one: the two kinds attach attestations by two
  different mechanisms, in-band for builds and as referrers for compositions. Unifying either
  direction was rejected — upward means streaming attestation bytes through the controller for
  content it never produced, downward means putting the attestation inside the artifact, which
  would end `output digest = f(spec)`.
- **SPDX, not CycloneDX**, because BuildKit emits SPDX. Publishing two formats from two kinds into
  one registry would make every consumer carry two readers, and the asymmetry between the kinds is
  large enough already.

### The one amendment to 0008: signatures use the `.sig` tag, not referrers

0008 said "signatures use the same rail". They do not, and the justification is 0008's own
sentence: **signing is theatre unless something verifies it.** The verifiers that exist —
policy-controller, Kyverno's `verifyImages`, Connaisseur — read cosign's `sha256-<hex>.sig` tag by
default; cosign's referrers mode is still experimental and is not what they look for. Choosing the
elegant rail over the one the verifier reads produces a signature nothing checks, which is exactly
the failure 0020 is about.

Attestations have no such constraint — nothing enforces on them — so they get referrers.

An unlooked-for benefit fell out: **a `.sig` is a tag**, so it is already covered by the registry's
`keepTags` retention policy. Only the untagged referrers needed anything new.

### Determinism is a requirement, not a nicety

An attestation payload must be a pure function of the same inputs the artifact's digest is a
function of. No timestamps, no UUIDs, no controller version, no hostname. The SBOM's
`documentNamespace` is derived from the output digest; `creationInfo.created` is the epoch; SLSA's
`runDetails.metadata` is omitted entirely.

`internal/oci/provenance_test.go` enforces the same rule one layer in, and the tests here cite it.
**The payoff is the elegant part:** because the payload is deterministic, "does this already exist"
is a comparison rather than a diff, and the steady reconcile loop costs nothing.

### Idempotence, which 0008 described in a way that would have cost a request per hour

0008 says "when the digest matches, the controller verifies the referrers exist and creates only
what is missing". Taken literally that is a registry round trip per object per interval, forever.

Instead there are two layers. `status.attestations` records the subject digest and the three
manifest digests, and is checked as one more conjunct after the input-hash and published-digest
checks a converged reconcile already performs — **zero extra requests**. The registry is consulted
only when that record cannot answer: a new digest, a feature just enabled, a restore.

Trusting a status field about the registry is safe here for a specific reason worth writing down:
the attestations live in the **same repository, under the same retention policy**, as an artifact
whose presence was just confirmed by the caller's own HEAD. A registry that lost the referrers lost
the artifact too, so the digest check fails first and everything is re-derived.

**First writer wins**, matched on predicate type rather than digest. The cost, stated rather than
hidden: an SBOM produced by a buggy version is never corrected in place. The alternative needs
`delete` on the registry — a permission this project has deliberately never held, so that no bug in
it can destroy an image. It is also why nothing in the payload may depend on the controller's
version: if it did, every upgrade would hit first-writer-wins on every object and the rule would
turn from correctness into permanent staleness.

## Consequences

**Enabling builder attestations changes the digest an unchanged `ImageBuild` publishes.** BuildKit
attaches attestations as extra manifests in an image **index**, so a single-platform build's output
becomes an index rather than a manifest. Existing pins keep resolving — the old digest is still
there — but a consumer that re-reads `status.artifact.digest` gets a different value, and anything
that assumed the digest named a manifest now finds an index. The options are in the input hash, so
it happens **once, visibly**, at the moment the operator turns the flag on.

**BuildKit's provenance carries wall-clock timestamps**, so the index digest differs on every run of
identical inputs. `rewrite-timestamp` and `SOURCE_DATE_EPOCH`, whose whole purpose is narrowing the
"same inputs, different bytes" gap, are **partly undone at the index level by this feature**. No
invariant breaks — [0025](0025-dockerfile-builds-as-a-second-kind.md) already says a build's output
is an observation — but it is a real trade and it belongs on the record.

**Referrers are untagged, and the retention guarantee had to be extended to reach them.** The
shipped policy is `deleteUntagged` with `keepUntagged.pulledWithin`, so an SBOM would have been
reclaimed one window after it was written, silently, while the image it describes lived on. That is
threat D6 on a new object type, in the deleting direction. The refresher now pulls each refreshed
digest's referrers, with a test that fails when the call site is removed — which the helper's own
tests did not catch.

**The dependency is small, and staying small needs a guard.** `sigstore/sigstore`'s signature and
cryptoutils packages, `go-securesystemslib/encrypted`, and hand-rolled SPDX, in-toto and DSSE
structs. Not `sigstore/cosign` (150–250 modules for about seventy-five lines of code), not the
`kms/*` subpackages (a cloud SDK each), and not `pkg/oauthflow`, which pulls a headless-Chrome
driver into a Kubernetes controller. Nor the SPDX and in-toto libraries: their struct tags would
decide our payload bytes, so a minor bump would re-attest every artifact in the cluster. There is a
`go.mod` drift guard naming each one.

### Four things that must be said plainly

1. **Signing is inert without admission verification.** Unchanged from 0008 — but this project now
   ships example Kyverno and policy-controller policies in `docs/examples/verify`, so it is a
   deployment step rather than an absence. That is the cheapest item in this work and the one that
   decides whether the feature is a feature.
2. **A signature here means "this operator produced this", and nothing more.** Any tenant who can
   create an `ImageComposition` in any namespace gets the operator's signature on whatever they
   composed. It attests provenance, not approval, and it is not a substitute for RBAC on the CRD.
3. **The passphrase protects the key file, not the key.** Both halves live in one Secret by default.
   What it buys is compatibility with cosign's format and protection against the key file leaking
   *without* the Secret — not a second factor.
4. **A key lost makes every existing artifact unverifiable; a key leaked makes every signature
   meaningless.** 0020 raised this and it is not being solved, only stated. There is no rotation
   story and no re-signing of history.

**The key never enters a build pod.** The builder signs the digest its Job reported, after the Job
has terminated. Code that came out of a git repository never runs in the same container as the
signing key — which is a better posture than shelling out to a cosign container inside the build
would have given, and a good post-hoc argument for the in-process choice.

## What is not built

Per-object signing keys; keyless; scanning the composer's output; referrers for the builder;
per-child signatures on an index; and PURL-level SPDX for `deb` layers. That last one is the best
future improvement in this area — `internal/oci/deb.go` already parses the control metadata, so this
project could emit a real package inventory that is *exact* rather than scanned, which almost
nothing else can. It is a second increment, and putting it in the first is how the first one does
not ship.
