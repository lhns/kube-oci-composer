# 0008. Supply chain: referrers for SBOM and signatures; key-based signing, not keyless

## Status

Proposed. Not implemented. Recorded now because the storage and API decisions around it are
already being made.

## Context

This controller publishes artifacts that other workloads run. That puts it in the supply chain,
and it should be able to say what it produced and prove it.

## Decision

**Attestations go into the registry as referrers, not into Kubernetes.**

OCI Distribution v1.1 has a `subject` field for exactly this: push the SBOM as its own artifact
whose subject is the image manifest digest, and the registry serves it from
`/v2/<name>/referrers/<digest>`. Signatures use the same rail. They travel with the artifact.

A ConfigMap is the wrong home: a ~1 MiB cap, and it separates the description from the thing
described so that copying the artifact loses its provenance.

**Determinism makes the provenance exact, and this is the strongest argument for the whole
design.** Every input is already pinned by digest, so the attestation states literally
"`core-1.1.1.tgz` sha256:… unpacked at `/core`, `s3-1.1.1.tgz` sha256:… at `/s3`, producing
sha256:…". No scanning, no inference, no irreproducible middle. That is a complete SLSA-style
provenance chain, which most build systems cannot honestly emit — an `ImageBuild` running
`apt-get install` fundamentally cannot, because its SBOM is a scan of the result and its
provenance stops at "we ran this Dockerfile".

**Key-based signing, not keyless.** Sigstore keyless publishes to the public Rekor transparency
log, which would put the names and digests of private images into a public ledger. A cosign key
pair in an encrypted Secret is simpler, works offline, and leaks nothing. Keyless remains an option
for genuinely public artifacts.

**Signing does not change the digest**, so the convergence check still short-circuits. When the
digest matches, the controller verifies the referrers exist and creates only what is missing.

## Consequences

**Signing is theatre unless something verifies it.** Producing signatures is half the job; without
admission verification (policy-controller, Kyverno, Connaisseur) an unsigned or mis-signed image is
still accepted. Until that exists the attestations are useful for audit and enforce nothing, and
the README must say so rather than claiming the supply chain is "secured".

In serving mode the Referrers API is available in the embedded registry but its storage does not
survive a restart (0013), so attestations there are best-effort until that is fixed. Anything
needing durable attestations should set `push` (0006).

## Alternatives rejected

**SBOM into a ConfigMap.** Size cap, and it detaches the description from the artifact.

**SBOM into `status`.** Worse: `status` has an etcd-sized budget shared with everything else, and
an SBOM is not status.

**Keyless signing by default.** Publishes private image names and digests to a public transparency
log. That is a surprising thing for a homelab controller to do, and it is not undoable.

**Scanning the produced artifact to generate the SBOM.** Available, and strictly worse here: the
inputs are already known exactly, so scanning would replace a precise statement with an inferred
one.
