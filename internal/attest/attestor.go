package attest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Attestor attaches an SBOM, provenance and a signature to an artifact, and declines to do it
// again.
type Attestor struct {
	SBOM       bool
	Provenance bool
	Key        *Key
}

// Enabled reports whether there is anything to do at all.
func (a *Attestor) Enabled() bool {
	return a != nil && (a.SBOM || a.Provenance || a.Key != nil)
}

// Record is what a controller stores in status so the next reconcile can tell there is nothing to
// do WITHOUT asking the registry (ADR 0008). The registry is consulted only when the record cannot
// answer.
//
// Trusting it is safe because the attestations share the artifact's repository and retention
// policy, and the caller has just confirmed the artifact exists: a registry that lost the
// referrers lost the artifact too, and the digest check fails first.
type Record struct {
	// Subject is the artifact digest these describe. A mismatch invalidates the whole record.
	Subject string `json:"subject,omitempty"`
	SBOM    string `json:"sbom,omitempty"`
	// Provenance is the SLSA referrer's manifest digest.
	Provenance string `json:"provenance,omitempty"`
	// Signature is the .sig manifest digest.
	Signature string `json:"signature,omitempty"`
}

// Complete reports whether this record already covers everything the Attestor is configured to do.
func (a *Attestor) Complete(rec *Record, subject string) bool {
	if !a.Enabled() {
		return true
	}
	if rec == nil || rec.Subject != subject {
		return false
	}
	if a.SBOM && rec.SBOM == "" {
		return false
	}
	if a.Provenance && rec.Provenance == "" {
		return false
	}
	if a.Key != nil && rec.Signature == "" {
		return false
	}
	return true
}

// Payloads carries what a controller knows about an artifact.
type Payloads struct {
	// BuildType names the producing kind, for SLSA.
	BuildType string
	// External and Internal are the SLSA parameter blocks. External should cover the same fields
	// the input hash covers, or provenance claims less than the artifact depends on.
	External any
	Internal any
	// Base is the base image, when there is one.
	Base *Source
	// Sources are the inputs, in SPEC ORDER.
	Sources []Source
}

// Ensure attaches whatever is missing and returns the record to store.
//
// FIRST WRITER WINS, matched on predicate type: an existing SBOM for this subject stays even if
// this controller would now produce different bytes. Correcting one in place would need registry
// delete permission, which this project never holds; fix a bad attestation by deleting the
// referrer by hand or changing the spec. This is also why no payload may depend on the
// controller's version.
func (a *Attestor) Ensure(
	ctx context.Context,
	repo name.Repository,
	subject v1.Descriptor,
	payloads Payloads,
	opts []remote.Option,
) (*Record, error) {
	if !a.Enabled() {
		return nil, nil
	}

	rec := &Record{Subject: subject.Digest.String()}
	repoName := repo.Name()

	existing, err := Existing(repo, subject.Digest, opts)
	if err != nil {
		return nil, err
	}

	if a.SBOM {
		if have, ok := existing[PredicateSPDX]; ok {
			rec.SBOM = have.String()
		} else {
			stmt := SPDXStatement(repoName, subject.Digest.String(), payloads.Base, payloads.Sources)
			digest, err := a.push(repo, subject, PredicateSPDX, stmt, opts)
			if err != nil {
				return nil, fmt.Errorf("attaching the SBOM: %w", err)
			}
			rec.SBOM = digest.String()
		}
	}

	if a.Provenance {
		if have, ok := existing[PredicateSLSA]; ok {
			rec.Provenance = have.String()
		} else {
			stmt := SLSAStatement(repoName, subject.Digest.String(), payloads.BuildType,
				payloads.External, payloads.Internal, payloads.Sources)
			digest, err := a.push(repo, subject, PredicateSLSA, stmt, opts)
			if err != nil {
				return nil, fmt.Errorf("attaching provenance: %w", err)
			}
			rec.Provenance = digest.String()
		}
	}

	if a.Key != nil {
		signed, have, err := a.Key.VerifiedSignatureExists(ctx, repo, subject.Digest, opts)
		if err != nil {
			return nil, fmt.Errorf("checking the signature: %w", err)
		}
		if signed {
			rec.Signature = have.String()
		} else {
			digest, err := a.Key.SignArtifact(ctx, repo, subject.Digest, opts)
			if err != nil {
				return nil, fmt.Errorf("signing: %w", err)
			}
			rec.Signature = digest.String()
		}
	}

	return rec, nil
}

// push encodes a statement, wraps it in a signed DSSE envelope when a key is configured (so it needs
// no separate .sig), and attaches it.
func (a *Attestor) push(repo name.Repository, subject v1.Descriptor, predicateType string, stmt Statement, opts []remote.Option) (v1.Hash, error) {
	body, err := json.Marshal(stmt)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("encoding the statement: %w", err)
	}

	signed := false
	if a.Key != nil {
		body, err = Envelope(a.Key, body)
		if err != nil {
			return v1.Hash{}, err
		}
		signed = true
	}
	return Push(repo, subject, predicateType, body, signed, opts)
}
