package attest

// SLSA v1.0 provenance, hand-rolled like the SPDX structs so the payload bytes stay ours to keep
// stable.

type slsaPredicate struct {
	BuildDefinition slsaBuildDefinition `json:"buildDefinition"`
	RunDetails      slsaRunDetails      `json:"runDetails"`
}

type slsaBuildDefinition struct {
	BuildType            string                   `json:"buildType"`
	ExternalParameters   any                      `json:"externalParameters"`
	InternalParameters   any                      `json:"internalParameters,omitempty"`
	ResolvedDependencies []slsaResourceDescriptor `json:"resolvedDependencies,omitempty"`
}

type slsaResourceDescriptor struct {
	Name        string            `json:"name,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Digest      map[string]string `json:"digest,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type slsaRunDetails struct {
	Builder slsaBuilder `json:"builder"`
	// Deliberately NO Metadata: invocationId, startedOn and finishedOn describe the run, and would
	// make the predicate differ on every reconcile of an unchanged object.
}

type slsaBuilder struct {
	// No version: an upgraded controller must not change the payload for an unchanged artifact.
	ID string `json:"id"`
}

// BuildTypeComposition names what produced an artifact; BuilderID names the builder.
const (
	BuildTypeComposition = "https://oci.lhns.de/ImageComposition/v1alpha1"
	BuilderID            = "https://github.com/lhns/kube-oci-composer"
)

// SLSAStatement describes how an artifact was produced.
//
// externalParameters should carry the same field set oci.InputHash covers (tested): narrower
// provenance would claim less than the artifact depends on (ADR 0026).
func SLSAStatement(repository, digest, buildType string, external, internal any, sources []Source) Statement {
	deps := make([]slsaResourceDescriptor, 0, len(sources))
	for _, s := range sources {
		d := slsaResourceDescriptor{Name: s.Name, URI: s.URI}
		if hex, ok := sha256Hex(s.Digest); ok {
			d.Digest = map[string]string{"sha256": hex}
		}
		if s.Target != "" {
			d.Annotations = map[string]string{"target": s.Target}
		}
		deps = append(deps, d)
	}

	return Statement{
		Type:          StatementType,
		PredicateType: PredicateSLSA,
		Subject:       []Subject{subjectFor(repository, digest)},
		Predicate: slsaPredicate{
			BuildDefinition: slsaBuildDefinition{
				BuildType:            buildType,
				ExternalParameters:   external,
				InternalParameters:   internal,
				ResolvedDependencies: deps,
			},
			RunDetails: slsaRunDetails{Builder: slsaBuilder{ID: BuilderID}},
		},
	}
}

// SPDXStatement wraps an SPDX document as an in-toto statement, so both predicates travel the same
// way and a consumer needs one code path to read either.
func SPDXStatement(repository, digest string, base *Source, sources []Source) Statement {
	return Statement{
		Type:          StatementType,
		PredicateType: PredicateSPDX,
		Subject:       []Subject{subjectFor(repository, digest)},
		Predicate:     SPDXDocument(repository, digest, base, sources),
	}
}

func subjectFor(repository, digest string) Subject {
	s := Subject{Name: repository, Digest: map[string]string{}}
	if hex, ok := sha256Hex(digest); ok {
		s.Digest["sha256"] = hex
	}
	return s
}

// sha256Hex returns the hex of a non-empty "sha256:<hex>" digest.
func sha256Hex(digest string) (string, bool) {
	if len(digest) > len("sha256:") && digest[:7] == "sha256:" {
		return digest[7:], true
	}
	return "", false
}
