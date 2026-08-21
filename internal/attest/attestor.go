package attest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Attestor attaches an SBOM, provenance and a signature to an artifact, and — more importantly —
// declines to do it again.
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
// do WITHOUT asking the registry.
//
// This is the whole idempotence design. ADR 0008 says "when the digest matches, the controller
// verifies the referrers exist and creates only what is missing" — which taken literally costs a
// registry round trip per object per interval, forever. Instead:
//
//	Layer 1: this record. Free. Checked as one more conjunct after the input-hash and
//	         published-digest checks a converged reconcile already performs.
//	Layer 2: the registry, consulted only when layer 1 cannot answer.
//
// Trusting a status field about the registry is safe here for a specific reason worth writing
// down: the attestations live in the SAME repository, under the SAME retention policy, as an
// artifact whose presence was just confirmed by the caller's own HEAD. A registry that lost the
// referrers lost the artifact too, so the digest check fails first and everything is re-derived.
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
	// External and Internal are the SLSA parameter blocks. External should cover the same field
	// set the input hash covers — provenance narrower than the hash claims less than the artifact
	// depends on.
	External any
	Internal any
	// Base is the base image, when there is one.
	Base *Source
	// Sources are the inputs, in SPEC ORDER.
	Sources []Source
}

// Ensure attaches whatever is missing and returns the record to store.
//
// Never re-attaches what is already there, and the rule for "already there" is FIRST WRITER WINS,
// matched on predicate type rather than on digest. If an SBOM already describes this subject, it
// stays, even if this controller would produce different bytes now.
//
// The cost of that rule, stated rather than hidden: an SBOM produced by a buggy version is never
// corrected in place. The alternative needs `delete` on the registry — a permission this project
// has deliberately never held, so that no bug in it can destroy an image — and it would rewrite a
// claim someone may already have recorded. Fixing a bad attestation means deleting the referrer by
// hand, or changing the spec so a new subject is produced.
//
// It is also why nothing in the payload may depend on the controller's version (see slsa.go): if it
// did, every upgrade would hit first-writer-wins on every object and the rule would turn from
// correctness into permanent staleness.
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

// push encodes a statement, wraps it in a DSSE envelope when a key is configured, and attaches it.
//
// The envelope is why the two switches stay genuinely independent: with a key, the attestation
// carries its own signature and needs no separate `.sig`; without one, the bare statement is the
// honest shape for "here are the facts, unsigned".
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
