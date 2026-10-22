package oci

import (
	"fmt"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// Threat-model gap R1: status.history records what produced an artifact, but only on the object.
// These annotations put it in the artifact itself.
//
// Manifest annotations rather than config labels, which would show up as the image's own
// application labels. Everything here is a pure function of the resolved inputs (ADR 0016), which
// is also why org.opencontainers.image.created is NOT set.
const (
	// AnnotationSources lists each layer as `name=digest` (or `name=revision` where the revision
	// identifies the content), space-separated, in spec order: a later layer overwrites an earlier
	// one, so order carries meaning.
	AnnotationSources = "de.lhns.oci-composer.sources"

	// AnnotationAssemblyVersion records the algorithm that produced these bytes, so an artifact
	// found in a registry can be matched against the code that made it.
	AnnotationAssemblyVersion = "de.lhns.oci-composer.assembly-version"

	// AnnotationBase names the base image's digest, or is absent for a scratch artifact. Nothing
	// else in the output says which image the base layers came from.
	AnnotationBase = "de.lhns.oci-composer.base"
)

// provenanceAnnotations describes the inputs an assembly consumed.
func provenanceAnnotations(base v1.Image, inputs []LayerInput) map[string]string {
	ann := map[string]string{
		AnnotationAssemblyVersion: fmt.Sprintf("%d", AssemblyVersion),
	}

	parts := make([]string, 0, len(inputs))
	for _, in := range inputs {
		id := in.identity()
		if id == "" {
			continue
		}
		parts = append(parts, in.Name+"="+id)
	}
	if len(parts) > 0 {
		ann[AnnotationSources] = strings.Join(parts, " ")
	}

	if base != nil {
		if d, err := base.Digest(); err == nil {
			ann[AnnotationBase] = d.String()
		}
		// An unreadable base digest only loses the annotation; the digest is in status anyway.
	}
	return ann
}

// withProvenance stamps the annotations onto the manifest.
func withProvenance(img v1.Image, base v1.Image, inputs []LayerInput) v1.Image {
	ann := provenanceAnnotations(base, inputs)
	if len(ann) == 0 {
		return img
	}
	// mutate.Annotations returns a v1.Image for a v1.Image input.
	out, ok := mutate.Annotations(img, ann).(v1.Image)
	if !ok {
		return img
	}
	return out
}
