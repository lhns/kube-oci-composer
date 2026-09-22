package attest

import (
	"fmt"
	"strings"
)

// SPDX 2.3, hand-rolled rather than via spdx/tools-golang, whose struct tags change between minor
// versions: a dependency bump would silently change the payload bytes and so every referrer digest.
// SPDX rather than CycloneDX because BuildKit's attest:sbom emits SPDX, so both kinds publish one
// format.

type spdxDocument struct {
	SPDXID            string             `json:"SPDXID"`
	SPDXVersion       string             `json:"spdxVersion"`
	DataLicense       string             `json:"dataLicense"`
	Name              string             `json:"name"`
	DocumentNamespace string             `json:"documentNamespace"`
	CreationInfo      spdxCreationInfo   `json:"creationInfo"`
	Packages          []spdxPackage      `json:"packages"`
	Relationships     []spdxRelationship `json:"relationships"`
}

type spdxCreationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type spdxPackage struct {
	SPDXID           string         `json:"SPDXID"`
	Name             string         `json:"name"`
	VersionInfo      string         `json:"versionInfo,omitempty"`
	DownloadLocation string         `json:"downloadLocation"`
	FilesAnalyzed    bool           `json:"filesAnalyzed"`
	Checksums        []spdxChecksum `json:"checksums,omitempty"`
	Comment          string         `json:"comment,omitempty"`
}

type spdxChecksum struct {
	Algorithm     string `json:"algorithm"`
	ChecksumValue string `json:"checksumValue"`
}

type spdxRelationship struct {
	SPDXElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSPDXElement string `json:"relatedSpdxElement"`
}

// SPDXDocument describes what went into an artifact.
//
// It is a manifest of INPUTS ("core-1.1.1.tgz sha256:… unpacked at /core"), not a package
// inventory: there are no CPEs (ADR 0008).
func SPDXDocument(repository string, digest string, base *Source, sources []Source) spdxDocument {
	doc := spdxDocument{
		SPDXID:      "SPDXRef-DOCUMENT",
		SPDXVersion: "SPDX-2.3",
		DataLicense: "CC0-1.0",
		Name:        repository,
		// Derived from the output digest, NOT a UUID, so the document is a pure function of the
		// artifact.
		DocumentNamespace: fmt.Sprintf("https://oci.lhns.de/spdx/%s@%s", repository, digest),
		CreationInfo: spdxCreationInfo{
			// The same epoch internal/oci stamps into every artifact.
			Created: "1970-01-01T00:00:00Z",
			// No version: an upgrade must not change the SBOM of an unchanged artifact.
			Creators: []string{"Tool: kube-oci-composer"},
		},
	}

	imageID := "SPDXRef-Image"
	doc.Packages = append(doc.Packages, spdxPackage{
		SPDXID:           imageID,
		Name:             repository,
		DownloadLocation: "NOASSERTION",
		FilesAnalyzed:    false,
		Checksums:        []spdxChecksum{{Algorithm: "SHA256", ChecksumValue: strings.TrimPrefix(digest, "sha256:")}},
	})
	doc.Relationships = append(doc.Relationships, spdxRelationship{
		SPDXElementID: "SPDXRef-DOCUMENT", RelationshipType: "DESCRIBES", RelatedSPDXElement: imageID,
	})

	if base != nil {
		doc.Packages = append(doc.Packages, spdxPackage{
			SPDXID:           "SPDXRef-Base",
			Name:             base.Name,
			VersionInfo:      base.Version,
			DownloadLocation: orNoAssertion(base.URI),
			FilesAnalyzed:    false,
			Checksums:        checksumOf(base.Digest),
		})
		doc.Relationships = append(doc.Relationships, spdxRelationship{
			SPDXElementID: imageID, RelationshipType: "GENERATED_FROM", RelatedSPDXElement: "SPDXRef-Base",
		})
	}

	// SPEC ORDER, never sorted: a later layer overwrites an earlier one (as in
	// internal/oci/provenance.go).
	for i, s := range sources {
		id := fmt.Sprintf("SPDXRef-Layer-%d-%s", i, sanitise(s.Name))
		doc.Packages = append(doc.Packages, spdxPackage{
			SPDXID:           id,
			Name:             s.Name,
			VersionInfo:      s.Version,
			DownloadLocation: orNoAssertion(s.URI),
			FilesAnalyzed:    false,
			Checksums:        checksumOf(s.Digest),
			Comment:          targetComment(s.Target),
		})
		doc.Relationships = append(doc.Relationships, spdxRelationship{
			SPDXElementID: imageID, RelationshipType: "CONTAINS", RelatedSPDXElement: id,
		})
	}
	return doc
}

func checksumOf(digest string) []spdxChecksum {
	if !strings.HasPrefix(digest, "sha256:") {
		return nil
	}
	return []spdxChecksum{{Algorithm: "SHA256", ChecksumValue: strings.TrimPrefix(digest, "sha256:")}}
}

func orNoAssertion(uri string) string {
	if uri == "" {
		return "NOASSERTION"
	}
	return uri
}

func targetComment(target string) string {
	if target == "" {
		return ""
	}
	return "unpacked at " + target
}

// sanitise keeps an SPDXID to the characters the specification allows.
func sanitise(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '-'
		}
	}, name)
}
