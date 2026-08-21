package attest

// SLSA v1.0 provenance, hand-rolled for the same reason the SPDX structs are: the payload bytes
// must be ours to keep stable.

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
	// NO Metadata field, and its absence is deliberate rather than an omission.
	//
	// SLSA v1.0 makes runDetails.metadata optional, and everything it would carry --
	// invocationId, startedOn, finishedOn -- is an observation of the RUN rather than a fact about
	// the artifact. Including any of them would make the predicate differ on every reconcile of an
	// unchanged object, so the controller would re-push provenance forever. The whole idempotence
	// design in attestor.go depends on this struct having no clock in it.
}

type slsaBuilder struct {
	// No version, for the same reason the SPDX creator has none: an upgraded controller must not
	// change the payload for an unchanged artifact.
	ID string `json:"id"`
}

// BuildTypeComposition and BuildTypeBuild name what produced an artifact.
const (
	BuildTypeComposition = "https://oci.lhns.de/ImageComposition/v1alpha1"
	BuilderID            = "https://github.com/lhns/kube-oci-composer"
)

// SLSAStatement describes how an artifact was produced.
//
// externalParameters should carry the same field set oci.InputHash covers. That correspondence is
// the design rule and there is a test for it: provenance narrower than the hash would claim less
// than the artifact actually depends on, which is the shape of ADR 0026's incident.
func SLSAStatement(repository, digest, buildType string, external, internal any, sources []Source) Statement {
	deps := make([]slsaResourceDescriptor, 0, len(sources))
	for _, s := range sources {
		d := slsaResourceDescriptor{Name: s.Name, URI: s.URI}
		if len(s.Digest) > len("sha256:") && s.Digest[:7] == "sha256:" {
			d.Digest = map[string]string{"sha256": s.Digest[7:]}
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
	if len(digest) > len("sha256:") && digest[:7] == "sha256:" {
		s.Digest["sha256"] = digest[7:]
	}
	return s
}
