# 62. This project's own releases are signed keyless

## Status

Accepted. Sits beside [0008](0008-supply-chain.md) and supersedes nothing: 0008 is about signing
what the controller composes, this is about signing the controller itself.

## Context

The release workflow publishes two images (`kube-oci-composer`, `kube-oci-builder`) and a chart to
ghcr. They were unsigned. 0008 rejects keyless signing for composed artifacts because it would put
private image names and digests into the public Rekor log. That reason does not apply here: this
repository, its images and its chart are public already, and the sibling operators sign theirs
keyless with the same workflow.

## Decision

**Tagged releases sign every image digest and the chart digest with cosign keyless, through
GitHub's OIDC token, and attest an SPDX SBOM per image.** The signing identity is the release
workflow at the tag, so no key exists to store, rotate or leak.

- `id-token: write` is granted to the `release` job only; the workflow default is `contents: read`,
  and the CI, e2e and upgrade gates it calls get no token.
- Dev builds are not signed: nothing verifies them, and each signature would be one more public
  Rekor entry and `.sig`/`.att` tags on ghcr. Their SBOM is still generated, so the step stays
  exercised.

## Consequences

Each release adds public Rekor entries naming its public digests. A verifier pins the workflow
identity (`https://github.com/lhns/kube-oci-composer/.github/workflows/release.yaml@refs/tags/<tag>`)
and the issuer `https://token.actions.githubusercontent.com`. As 0008 says, a signature enforces
nothing until something verifies it at admission.

## Alternatives rejected

**A cosign key, as 0008 chooses for composed artifacts.** It protects nothing that is not public
already, and adds a secret to store and rotate.
