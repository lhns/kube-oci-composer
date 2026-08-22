# 0020. Is the supply-chain work worth building?

## Status

**Closed by [0040](0040-the-supply-chain-work-is-worth-building.md)**, in favour of option A.

Two of the three things that held this open were resolved by work done for other reasons:
[0035](0035-a-registry-is-the-only-publication-path.md) removed the embedded registry, so the
referrers objection below has no referent and option D collapsed into option A. What remained --
that signing changes nothing until something verifies it -- is answered by shipping example
admission policies rather than by argument.

The original question, for the record: [ADR 0008](0008-supply-chain.md) designs SBOM, provenance and signing and has been
Proposed since it was written. Nothing is implemented. The question is not how to build it — 0008
answers that — but whether to.

## Context

0008 makes a genuinely strong argument that this project can emit *exact* provenance rather than
scanned provenance: every input is already pinned by digest, so the attestation states precisely
what went in and what came out. Most build systems cannot do that honestly.

It also contains the sentence that keeps the question open:

> Signing is theatre unless something verifies it.

Producing signatures changes nothing on its own. Until an admission controller refuses an unsigned
or mis-signed image, the artifacts are decoration. So the real scope is larger than 0008 describes:
it is signing *plus* admission verification *plus* the operational burden of a key that, if lost,
makes every existing artifact unverifiable, and if leaked, makes signatures meaningless.

There is a second problem 0008 acknowledges but does not resolve. Attestations attach to a
manifest through the Referrers API. In the default serving mode that API is provided by the
embedded registry, whose storage is not designed for this, and whose referrers implementation
linear-scans and re-parses every manifest in the repository per request. So the mode most users
run is the mode least able to hold attestations.

## Options

**A. Build it as designed.** SBOM and provenance as referrers, key-based signing with a
SOPS-encrypted cosign key, and document that verification is the user's job.

- Honest about the limits, and the provenance is genuinely better than a scanner's.
- Ships a feature whose security value is zero until something else is installed.

**B. Build provenance and SBOM only, no signing.** The attestation is useful for audit and answers
"what is in this artifact" without pretending to be a security control. Sidesteps key management
entirely.

- Loses the tamper-evidence story, which is the part people ask for.
- Arguably the most honest split: state facts, do not claim guarantees.

**C. Build nothing, and say so.** The inputs are digest-pinned and the output digest is a pure
function of the spec, so anyone can independently reproduce the artifact and compare. That is a
stronger property than most signed images have, and it needs no key.

- The reproducibility argument is real and under-used. It is also not what a compliance checklist
  asks for.

**D. Push-mode only.** Attestations require a registry that implements referrers properly, so
support them when `spec.push` is set and refuse otherwise. Narrow, and avoids leaning on the
embedded registry for something it is poor at.

## What would decide it

- **Does anything verify?** If no admission controller is going to be installed, A is decoration
  and B or C is the honest choice. This is a deployment decision, not a code one.
- **Does anyone ask for an SBOM?** The audience for B is compliance, and there may not be one.
- **How badly does the embedded registry's referrers implementation behave** with a realistic
  number of manifests. If it degrades, D follows regardless of the rest.

## Consequences of leaving it open

The README lists this as not implemented and says signing would be theatre until something
verifies it, which is accurate. The risk is drift the other way: ADR 0008 reads as a plan of
record, and a reader may assume it is coming. It is not, until this is answered.
