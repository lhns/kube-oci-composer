// Package attest builds and attaches supply-chain statements: an SBOM, SLSA provenance, and a
// cosign-compatible signature.
//
// Everything here is a PURE FUNCTION of inputs that already move the artifact's digest. That is not
// a stylistic preference, it is the constraint the whole design rests on:
//
//   - `output digest = f(spec)` is the project's core invariant (ADR 0016), and an attestation
//     carrying a build time, a hostname or a controller version would not break the artifact's
//     digest but would break the ATTESTATION's, so a steady reconcile loop would re-push one every
//     interval, forever.
//   - Because the payload is deterministic, "does this already exist" is a comparison rather than a
//     diff, which is what makes the idempotence check in attestor.go cost nothing.
//
// So: no timestamps, no UUIDs, no controller version, no hostname. `internal/oci/provenance_test.go`
// enforces the same rule one layer in, and the tests here cite it.
package attest

// Source is one input that went into an artifact, in terms both controllers can supply.
//
// Deliberately not oci.LayerInput: this package is used by two controllers whose inputs differ (the
// composer has many layers, a build has exactly one context), and depending on either one's types
// would make the other awkward.
type Source struct {
	// Name is the layer's name in the spec, or "context" for a build.
	Name string
	// URI is where it came from, when there is one to state.
	URI string
	// Digest is the content digest, "sha256:...".
	Digest string
	// Version is a human-facing revision when the digest is not what identifies the content -- a
	// Flux revision, say. Preferred over Digest in the SBOM for the same reason InputHash prefers
	// it: source-controller re-packs on restart, so the tarball digest moves while the revision
	// does not.
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

// Predicate types, as the in-toto and SPDX specifications name them. These are the keys consumers
// filter on -- `cosign download attestation`, `oras discover`, and BuildKit's own attestations all
// use the same annotation, which is why one key serves both kinds even though they attach
// differently.
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
