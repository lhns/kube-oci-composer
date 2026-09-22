// Package attest builds and attaches supply-chain statements: an SBOM, SLSA provenance, and a
// cosign-compatible signature.
//
// Every payload is a PURE FUNCTION of inputs that already move the artifact's digest (ADR 0016): no
// timestamps, UUIDs, controller version or hostname. Otherwise a steady reconcile loop would re-push
// attestations forever, and "does this already exist" could not be a cheap comparison.
// internal/oci/provenance_test.go enforces the same rule one layer in.
package attest

// Source is one input that went into an artifact, in terms both controllers can supply.
//
// Not oci.LayerInput: both controllers use this package, and their inputs differ.
type Source struct {
	// Name is the layer's name in the spec, or "context" for a build.
	Name string
	// URI is where it came from, when there is one to state.
	URI string
	// Digest is the content digest, "sha256:...".
	Digest string
	// Version is a revision that identifies the content better than Digest, such as a Flux
	// revision (source-controller re-packs on restart, moving the tarball digest). Preferred over
	// Digest in the SBOM, as in InputHash.
	Version string
	// Target is where the content landed inside the image, when that is meaningful.
	Target string
}

// Identity is what names this source's content: the revision when there is one, else the digest.
func (s Source) Identity() string {
	if s.Version != "" {
		return s.Version
	}
	return s.Digest
}

// Predicate types, as the in-toto and SPDX specifications name them. Consumers (cosign, oras,
// BuildKit) filter on these under one annotation key.
const (
	PredicateSPDX = "https://spdx.dev/Document"
	PredicateSLSA = "https://slsa.dev/provenance/v1"

	// MediaTypeInToto is the layer media type for an unsigned statement.
	MediaTypeInToto = "application/vnd.in-toto+json"
	// MediaTypeDSSE is the layer media type once the statement is wrapped in a signed envelope.
	MediaTypeDSSE = "application/vnd.dsse.envelope.v1+json"
	// AnnotationPredicateType is how a consumer tells the two predicates apart.
	AnnotationPredicateType = "in-toto.io/predicate-type"
)

// Statement is an in-toto v1 statement.
type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     any       `json:"predicate"`
}

// Subject names what a statement is about.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// StatementType is the in-toto v1 statement type.
const StatementType = "https://in-toto.io/Statement/v1"
